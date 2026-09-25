package integration_real

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/repo"
	"repolens/internal/snapshot"
)

func setupRealMySQL(t *testing.T, migrationDir ...string) (*gorm.DB, *jobs.Store, func()) {
	ctx := context.Background()

	mysqlContainer, err := tcmysql.RunContainer(ctx,
		tc.WithImage("mysql:8.0"),
		tcmysql.WithDatabase("repolens_test"),
		tcmysql.WithUsername("testuser"),
		tcmysql.WithPassword("testpass"),
	)
	if err != nil {
		if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "1" {
			t.Fatalf("FAILED: real MySQL testcontainers required by release gate but failed to start: %v", err)
		}
		t.Skipf("Skipping real MySQL testcontainers test (Docker not available: %v)", err)
		return nil, nil, nil
	}

	connStr, err := mysqlContainer.ConnectionString(ctx, "charset=utf8mb4&parseTime=True&loc=Local")
	if err != nil {
		_ = mysqlContainer.Terminate(ctx)
		t.Fatalf("failed to get connection string: %v", err)
	}

	db, err := gorm.Open(gormmysql.Open(connStr), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		_ = mysqlContainer.Terminate(ctx)
		t.Fatalf("failed to open real MySQL connection: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		_ = mysqlContainer.Terminate(ctx)
		t.Fatalf("failed getting sql.DB: %v", err)
	}

	migrationsPath := filepath.Join("..", "..", "migrations")
	if len(migrationDir) > 0 {
		migrationsPath = migrationDir[0]
	}
	if err := mysql.ApplyMigrations(&mysql.DB{GormDB: db, SqlDB: sqlDB}, migrationsPath); err != nil {
		_ = mysqlContainer.Terminate(ctx)
		t.Fatalf("failed to apply authoritative MySQL migrations: %v", err)
	}

	jobsStore := jobs.NewStore(sqlDB)

	cleanup := func() {
		_ = mysqlContainer.Terminate(context.Background())
	}

	return db, jobsStore, cleanup
}

func TestRealMySQL_Migration009ResumesAfterPartialDDL(t *testing.T) {
	ctx := context.Background()
	container, err := tcmysql.RunContainer(ctx,
		tc.WithImage("mysql:8.0"),
		tcmysql.WithDatabase("repolens_migration_test"),
		tcmysql.WithUsername("testuser"),
		tcmysql.WithPassword("testpass"),
	)
	if err != nil {
		if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "1" {
			t.Fatalf("FAILED: real MySQL migration recovery test required but container failed to start: %v", err)
		}
		t.Skipf("Skipping real MySQL migration recovery test (Docker not available: %v)", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	connStr, err := container.ConnectionString(ctx, "charset=utf8mb4&parseTime=True&loc=Local")
	if err != nil {
		t.Fatalf("get MySQL connection string: %v", err)
	}
	db, err := gorm.Open(gormmysql.Open(connStr), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get SQL connection: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	migrationDB := &mysql.DB{GormDB: db, SqlDB: sqlDB}

	// Bring the database to the schema immediately before 009, then simulate a
	// process dying after MySQL has committed some (but not all) ALTER TABLEs.
	allMigrations := filepath.Join("..", "..", "migrations")
	partialDir := t.TempDir()
	entries, err := os.ReadDir(allMigrations)
	if err != nil {
		t.Fatalf("read migration directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "009_v2_2_rc_recovery.sql" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(allMigrations, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(partialDir, entry.Name()), contents, 0o600); err != nil {
			t.Fatalf("copy %s: %v", entry.Name(), err)
		}
	}
	if err := mysql.ApplyMigrations(migrationDB, partialDir); err != nil {
		t.Fatalf("apply migrations 001-008: %v", err)
	}

	started := time.Now().UTC().Truncate(time.Millisecond)
	for _, id := range []string{"partial-attempt-1", "partial-attempt-2"} {
		if err := db.Exec(`INSERT INTO diagnosis_attempts
			(id, diagnosis_run_id, attempt_no, worker_id, started_at, heartbeat_at, deadline_at, raw_output)
			VALUES (?, 'partial-run', 1, 'legacy-worker', ?, ?, ?, 'legacy provider output')`,
			id, started, started, started.Add(time.Minute)).Error; err != nil {
			t.Fatalf("seed legacy attempt %s: %v", id, err)
		}
	}
	if err := db.Exec(`ALTER TABLE diagnosis_attempts ADD COLUMN execution_generation INT NOT NULL DEFAULT 1 AFTER diagnosis_run_id`).Error; err != nil {
		t.Fatalf("simulate first committed ALTER: %v", err)
	}
	if err := db.Exec(`ALTER TABLE diagnosis_attempts ADD COLUMN checkpoint_kind VARCHAR(32) NOT NULL DEFAULT 'NONE' AFTER parsed_report_draft_json`).Error; err != nil {
		t.Fatalf("simulate second committed ALTER: %v", err)
	}

	// The duplicate legacy identities make the unique-index step fail after the
	// remaining DDL and backfill have committed. The failed migration must not
	// be recorded, and a later retry must resume without repeating ALTERs.
	if err := mysql.ApplyMigrations(migrationDB, allMigrations); err == nil {
		t.Fatal("expected duplicate legacy Attempt identities to prevent migration 009")
	}
	var attemptCount int
	if err := db.Raw(`SELECT COUNT(*) FROM diagnosis_attempts WHERE diagnosis_run_id = 'partial-run'`).Scan(&attemptCount).Error; err != nil {
		t.Fatalf("count legacy attempts after failed migration: %v", err)
	}
	if attemptCount != 2 {
		t.Fatalf("failed migration modified legacy rows: got %d, want 2", attemptCount)
	}
	var checkpointKinds int
	if err := db.Raw(`SELECT COUNT(*) FROM diagnosis_attempts WHERE diagnosis_run_id = 'partial-run' AND checkpoint_kind = 'LEGACY_UNTYPED'`).Scan(&checkpointKinds).Error; err != nil {
		t.Fatalf("verify committed backfill: %v", err)
	}
	if checkpointKinds != 2 {
		t.Fatalf("backfill before interrupted unique-index step: got %d, want 2", checkpointKinds)
	}
	var migrationRecorded int
	if err := db.Raw(`SELECT COUNT(*) FROM schema_migrations WHERE version = '009_v2_2_rc_recovery.sql'`).Scan(&migrationRecorded).Error; err != nil {
		t.Fatalf("check migration record after failure: %v", err)
	}
	if migrationRecorded != 0 {
		t.Fatal("failed migration 009 was recorded as applied")
	}
	if err := db.Exec(`DELETE FROM diagnosis_attempts WHERE id = 'partial-attempt-2'`).Error; err != nil {
		t.Fatalf("remove conflicting fixture row: %v", err)
	}
	if err := mysql.ApplyMigrations(migrationDB, allMigrations); err != nil {
		t.Fatalf("resume partially committed migration 009: %v", err)
	}
	if err := db.Raw(`SELECT COUNT(*) FROM schema_migrations WHERE version = '009_v2_2_rc_recovery.sql'`).Scan(&migrationRecorded).Error; err != nil {
		t.Fatalf("check completed migration record: %v", err)
	}
	if migrationRecorded != 1 {
		t.Fatalf("migration 009 record count: got %d, want 1", migrationRecorded)
	}
	var uniqueIndexParts int
	if err := db.Raw(`SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'diagnosis_attempts' AND INDEX_NAME = 'uq_attempt_run_generation_no' AND NON_UNIQUE = 0`).Scan(&uniqueIndexParts).Error; err != nil {
		t.Fatalf("verify recovered unique index: %v", err)
	}
	if uniqueIndexParts != 3 {
		t.Fatalf("recovered unique index columns: got %d, want 3", uniqueIndexParts)
	}
}

func TestRealMySQL_Migration013ResumesAndScopesSnapshotIdentityByRevision(t *testing.T) {
	ctx := context.Background()
	container, err := tcmysql.RunContainer(ctx,
		tc.WithImage("mysql:8.0"),
		tcmysql.WithDatabase("repolens_snapshot_migration_test"),
		tcmysql.WithUsername("testuser"),
		tcmysql.WithPassword("testpass"),
	)
	if err != nil {
		if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "1" {
			t.Fatalf("FAILED: real MySQL migration recovery test required but container failed to start: %v", err)
		}
		t.Skipf("Skipping real MySQL migration recovery test (Docker not available: %v)", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	connStr, err := container.ConnectionString(ctx, "charset=utf8mb4&parseTime=True&loc=Local")
	if err != nil {
		t.Fatalf("get MySQL connection string: %v", err)
	}
	db, err := gorm.Open(gormmysql.Open(connStr), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open MySQL: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get SQL connection: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	migrationDB := &mysql.DB{GormDB: db, SqlDB: sqlDB}

	allMigrations := filepath.Join("..", "..", "migrations")
	pre013Dir := t.TempDir()
	entries, err := os.ReadDir(allMigrations)
	if err != nil {
		t.Fatalf("read migration directory: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "013_v2_2_revision_snapshot_identity.sql" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(allMigrations, entry.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(pre013Dir, entry.Name()), contents, 0o600); err != nil {
			t.Fatalf("copy %s: %v", entry.Name(), err)
		}
	}
	if err := mysql.ApplyMigrations(migrationDB, pre013Dir); err != nil {
		t.Fatalf("apply migrations 001-012: %v", err)
	}
	const repositoryID = "repo-snapshot-identity"
	const commitSHA = "0123456789012345678901234567890123456789"
	if err := db.Exec(`INSERT INTO repository_snapshots
		(id, repository_id, analysis_revision_id, commit_sha, ref, materialized_path, content_hash, status)
		VALUES ('legacy-ready-snapshot', ?, 'legacy-ready-revision', ?, 'main', '/tmp/legacy-source', 'legacy-hash', 'READY')`,
		repositoryID, commitSHA).Error; err != nil {
		t.Fatalf("seed legacy READY snapshot: %v", err)
	}

	// Simulate an interrupted migration after the restrictive legacy unique
	// index has been removed but before the replacement index is created.
	if err := db.Exec(`DROP INDEX uq_snapshot_repo_commit ON repository_snapshots`).Error; err != nil {
		t.Fatalf("simulate partial migration 013: %v", err)
	}
	if err := mysql.ApplyMigrations(migrationDB, allMigrations); err != nil {
		t.Fatalf("resume partial migration 013: %v", err)
	}

	var migrationRecorded int
	if err := db.Raw(`SELECT COUNT(*) FROM schema_migrations WHERE version = '013_v2_2_revision_snapshot_identity.sql'`).Scan(&migrationRecorded).Error; err != nil {
		t.Fatalf("check migration record: %v", err)
	}
	if migrationRecorded != 1 {
		t.Fatalf("migration 013 record count = %d, want 1", migrationRecorded)
	}
	var existingCount int
	if err := db.Raw(`SELECT COUNT(*) FROM repository_snapshots WHERE id = 'legacy-ready-snapshot' AND status = 'READY' AND analysis_revision_id = 'legacy-ready-revision'`).Scan(&existingCount).Error; err != nil {
		t.Fatalf("verify legacy READY snapshot: %v", err)
	}
	if existingCount != 1 {
		t.Fatal("migration did not preserve the historical READY snapshot")
	}
	if err := db.Exec(`INSERT INTO repository_snapshots
		(id, repository_id, analysis_revision_id, commit_sha, ref, materialized_path, content_hash, status)
		VALUES ('current-snapshot', ?, 'current-v22-revision', ?, 'main', '/tmp/current-source', 'current-hash', 'MATERIALIZING')`,
		repositoryID, commitSHA).Error; err != nil {
		t.Fatalf("insert new revision snapshot for same commit: %v", err)
	}
	if err := db.Exec(`INSERT INTO repository_snapshots
		(id, repository_id, analysis_revision_id, commit_sha, ref, materialized_path, content_hash, status)
		VALUES ('duplicate-current-snapshot', ?, 'current-v22-revision', ?, 'main', '/tmp/duplicate-source', 'duplicate-hash', 'MATERIALIZING')`,
		repositoryID, commitSHA).Error; err == nil {
		t.Fatal("duplicate snapshot within one AnalysisRevision was accepted")
	}
	var oldIndexCount, newIndexParts int
	if err := db.Raw(`SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'repository_snapshots' AND INDEX_NAME = 'uq_snapshot_repo_commit'`).Scan(&oldIndexCount).Error; err != nil {
		t.Fatalf("verify legacy index removal: %v", err)
	}
	if oldIndexCount != 0 {
		t.Fatal("legacy repository+commit unique index remains after migration")
	}
	if err := db.Raw(`SELECT COUNT(*) FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'repository_snapshots' AND INDEX_NAME = 'uq_snapshot_repo_commit_revision' AND NON_UNIQUE = 0`).Scan(&newIndexParts).Error; err != nil {
		t.Fatalf("verify revision-scoped index: %v", err)
	}
	if newIndexParts != 3 {
		t.Fatalf("revision-scoped unique index columns = %d, want 3", newIndexParts)
	}
}

func TestRealMySQL_DiagnosisIdempotencyAndJob(t *testing.T) {
	db, jobsStore, cleanup := setupRealMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	diagSvc := diagnosis.NewService(diagStore, repoStore, snapStore)

	testRepo := &repo.Repository{
		ID:         "repo-real-mysql",
		UserID:     "user-1",
		Name:       "payment-svc",
		GitURL:     "https://github.com/example/payment-svc",
		DefaultRef: "main",
		Status:     "ACTIVE",
	}
	_ = repoStore.Create(ctx, testRepo)

	testSnap := &snapshot.RepositorySnapshot{
		ID:           "snap-real-mysql",
		RepositoryID: testRepo.ID,
		CommitSHA:    "abc123456789",
		Ref:          "main",
		Status:       snapshot.StatusReady,
	}
	_ = snapStore.Create(ctx, testSnap)

	input := diagnosis.CreateDiagnosisInput{
		UserID:           testRepo.UserID,
		RepositoryID:     testRepo.ID,
		SnapshotID:       testSnap.ID,
		IssueTitle:       "Deadlock in payment handler",
		IssueDescription: "Two concurrent transactions acquire row locks in reverse order",
		ErrorLog:         "Error 1213: Deadlock found when trying to get lock",
		IdempotencyKey:   "idemp-real-001",
		CodeIndexBuildID: 3001,
		RetrievalBuildID: 4001,
	}

	// 1. Create DiagnosisRun and AnalysisJob transactionally on real MySQL
	run1, created1, err := diagSvc.Create(ctx, input)
	if err != nil || !created1 {
		t.Fatalf("first creation failed on real MySQL: %v", err)
	}
	if run1.Status != diagnosis.StatusQueued {
		t.Errorf("expected QUEUED status, got %s", run1.Status)
	}

	// Verify AnalysisJob exists on MySQL in PENDING status
	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run1.ID)
	if err != nil || job == nil {
		t.Fatalf("expected analysis_job on MySQL, got err: %v", err)
	}
	if job.Status != jobs.StatusPending {
		t.Errorf("expected job status PENDING, got %s", job.Status)
	}

	// 2. Duplicate submission with SAME payload -> Return existing Run
	run2, created2, err := diagSvc.Create(ctx, input)
	if err != nil || created2 {
		t.Fatalf("expected duplicate recognized, but created2=%v (err=%v)", created2, err)
	}
	if run2.ID != run1.ID {
		t.Errorf("expected returned run ID %s to match %s", run2.ID, run1.ID)
	}

	// 3. Duplicate submission with DIFFERENT payload -> 409 Conflict
	conflictInput := input
	conflictInput.IssueTitle = "Mismatched title for conflict check"
	_, _, errConflict := diagSvc.Create(ctx, conflictInput)
	if !errors.Is(errConflict, diagnosis.ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict on real MySQL, got: %v", errConflict)
	}
}

func TestRealMySQL_ConcurrentSkipLockedClaim(t *testing.T) {
	db, jobsStore, cleanup := setupRealMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)

	// Create 10 distinct diagnosis runs on MySQL
	for i := 1; i <= 10; i++ {
		run := &diagnosis.DiagnosisRun{
			ID:                     fmt.Sprintf("diag-real-batch-%d", i),
			UserID:                 "user-conc-mysql",
			RepositoryID:           "repo-conc-mysql",
			SnapshotID:             "snap-conc-mysql",
			IssueTitle:             fmt.Sprintf("Batch Issue %d", i),
			IdempotencyKey:         fmt.Sprintf("k-conc-mysql-%d", i),
			IdempotencyRequestHash: fmt.Sprintf("h-conc-mysql-%d", i),
		}
		_ = diagStore.Create(ctx, run)
	}

	// Simulate 5 workers concurrently claiming 2 jobs each with FOR UPDATE SKIP LOCKED
	numWorkers := 5
	claimedPerWorker := make([][]*jobs.AnalysisJob, numWorkers)
	var wg sync.WaitGroup

	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		wIdx := i
		workerID := fmt.Sprintf("worker-node-%d", wIdx)
		go func() {
			defer wg.Done()
			claimed, err := jobsStore.ClaimJobs(ctx, workerID, 2, 30*time.Second)
			if err == nil {
				claimedPerWorker[wIdx] = claimed
			}
		}()
	}
	wg.Wait()

	// Verify all 10 jobs were claimed with ZERO duplicate claims across workers
	seenJobIDs := make(map[int64]string)
	totalClaimed := 0
	for wIdx, claimed := range claimedPerWorker {
		for _, cj := range claimed {
			totalClaimed++
			if prevWorker, duplicate := seenJobIDs[cj.ID]; duplicate {
				t.Fatalf("DUPLICATE CLAIM DETECTED on MySQL: job %d claimed by both %s and %d", cj.ID, prevWorker, wIdx)
			}
			seenJobIDs[cj.ID] = fmt.Sprintf("worker-%d", wIdx)
		}
	}

	if totalClaimed != 10 {
		t.Fatalf("expected all 10 jobs claimed across workers, got %d", totalClaimed)
	}
}

func TestRealMySQL_LeaseRenewalAndReaping(t *testing.T) {
	db, jobsStore, cleanup := setupRealMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)

	run := &diagnosis.DiagnosisRun{
		ID:                     "diag-reap-test",
		UserID:                 "user-reap",
		RepositoryID:           "repo-reap",
		SnapshotID:             "snap-reap",
		IssueTitle:             "Reap Test",
		IdempotencyKey:         "k-reap",
		IdempotencyRequestHash: "h-reap",
	}
	_ = diagStore.Create(ctx, run)

	// Claim with a short lease (500ms)
	claimed, err := jobsStore.ClaimJobs(ctx, "worker-crashing", 1, 500*time.Millisecond)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim failed: %v", err)
	}

	// Wait for lease expiration
	time.Sleep(700 * time.Millisecond)

	// Reaper runs on real MySQL
	reaped, err := jobsStore.ReapExpiredJobs(ctx, 10)
	if err != nil {
		t.Fatalf("reap failed on real MySQL: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 reaped job on MySQL, got %d", reaped)
	}

	reapedJob, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatalf("failed fetching reaped job: %v", err)
	}
	if reapedJob.Status != jobs.StatusRetryWait {
		t.Errorf("expected job in RETRY_WAIT after reaping on MySQL, got %s", reapedJob.Status)
	}
}
