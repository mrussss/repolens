package integration_real

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"repolens/internal/analysispipeline"
	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/repo"
	"repolens/internal/revision"
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

type mysqlTransientFinalizeStore struct {
	*jobs.Store
	calls     atomic.Int32
	completed chan error
}

func (s *mysqlTransientFinalizeStore) ConditionalFinalizeSuccess(ctx context.Context, id int64, workerID, claimToken string) error {
	if s.calls.Add(1) == 1 {
		return errors.New("injected transient finalization database error")
	}
	err := s.Store.ConditionalFinalizeSuccess(ctx, id, workerID, claimToken)
	s.completed <- err
	return err
}

func TestRealMySQL_WorkerRetriesTransientTerminalFinalization(t *testing.T) {
	_, store, cleanup := setupRealMySQL(t)
	if store == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	job := &jobs.AnalysisJob{
		JobType: jobs.JobTypeBuildCodeIndex, ResourceID: "mysql-finalization-retry", MaxAttempts: 3,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	wrapped := &mysqlTransientFinalizeStore{Store: store, completed: make(chan error, 1)}
	cfg := jobs.DefaultWorkerConfig()
	cfg.WorkerID = "mysql-finalize-retry-worker"
	cfg.Concurrency = 1
	cfg.BatchSize = 1
	cfg.PollInterval = 10 * time.Millisecond
	cfg.LeaseDuration = 5 * time.Second
	cfg.ReapInterval = time.Hour
	worker := jobs.NewWorker(wrapped, cfg)
	worker.RegisterHandler(jobs.JobTypeBuildCodeIndex, jobs.HandlerFunc(func(context.Context, *jobs.AnalysisJob) error {
		return nil
	}))
	worker.Start(ctx)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = worker.StopGracefully(stopCtx)
	})

	select {
	case err := <-wrapped.completed:
		if err != nil {
			t.Fatalf("second MySQL finalization: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not retry MySQL terminal finalization")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := worker.StopGracefully(stopCtx); err != nil {
		t.Fatalf("StopGracefully after durable MySQL finalization: %v", err)
	}

	saved, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != jobs.StatusSucceeded || saved.AttemptCount != 1 || !saved.ExecutionStarted {
		t.Fatalf("MySQL finalization state=%+v; want SUCCEEDED with one started attempt", saved)
	}
	if got := wrapped.calls.Load(); got != 2 {
		t.Fatalf("MySQL success finalization calls=%d; want injected failure plus successful retry", got)
	}
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

func TestRealMySQL_ExecutionStartMigrationUpgradeAndBoundary(t *testing.T) {
	allMigrations := filepath.Join("..", "..", "migrations")
	pre016Dir := t.TempDir()
	entries, err := os.ReadDir(allMigrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "016_v2_2_execution_start_boundary.sql" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(allMigrations, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pre016Dir, entry.Name()), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	db, jobsStore, cleanup := setupRealMySQL(t, pre016Dir)
	if db == nil {
		return
	}
	defer cleanup()
	ctx := context.Background()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO analysis_jobs
		(job_type, resource_id, status, attempt_count, max_attempts, next_run_at, worker_id, claim_token, lease_until)
		VALUES ('BUILD_CODE_INDEX', 'legacy-running-before-016', 'RUNNING', 2, 3, UTC_TIMESTAMP(3), 'legacy-worker', 'legacy-token', DATE_ADD(UTC_TIMESTAMP(3), INTERVAL 1 MINUTE))`); err != nil {
		t.Fatalf("seed legacy RUNNING job before 016: %v", err)
	}
	if err := mysql.ApplyMigrations(&mysql.DB{GormDB: db, SqlDB: sqlDB}, allMigrations); err != nil {
		t.Fatalf("upgrade schema through migration 016: %v", err)
	}
	var legacyStarted bool
	var legacyAttempt int
	if err := sqlDB.QueryRowContext(ctx, `SELECT execution_started, attempt_count FROM analysis_jobs WHERE resource_id = 'legacy-running-before-016'`).Scan(&legacyStarted, &legacyAttempt); err != nil {
		t.Fatal(err)
	}
	if !legacyStarted || legacyAttempt != 2 {
		t.Fatalf("legacy RUNNING migration state = started %t / attempt %d, want true / 2", legacyStarted, legacyAttempt)
	}
	var migrationRecords int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = '016_v2_2_execution_start_boundary.sql'`).Scan(&migrationRecords); err != nil || migrationRecords != 1 {
		t.Fatalf("migration 016 records = %d err=%v; want one", migrationRecords, err)
	}

	job := &jobs.AnalysisJob{JobType: jobs.JobTypeBuildCodeIndex, ResourceID: "fresh-job-start-boundary", AttemptCount: 0, MaxAttempts: 3}
	if err := jobsStore.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "fresh-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].AttemptCount != 0 || claimed[0].ExecutionStarted {
		t.Fatalf("fresh MySQL claim = %+v err=%v; want attempt 0 and not started", claimed, err)
	}
	attempt, err := jobsStore.MarkExecutionStarted(ctx, job.ID, "fresh-worker", *claimed[0].ClaimToken, claimed[0].ExecutionGeneration)
	if err != nil || attempt != 1 {
		t.Fatalf("fresh MySQL execution start = %d err=%v; want attempt 1", attempt, err)
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
	diagSvc := diagnosis.NewService(diagnosis.ServiceDependencies{Store: diagStore, RepoStore: repoStore, Lineage: analysispipeline.NewResolver(revision.NewStore(db), snapStore, nil)})

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

func TestRealMySQL_ReturnUndispatchedClaimIsClaimFenced(t *testing.T) {
	_, jobsStore, cleanup := setupRealMySQL(t)
	if jobsStore == nil {
		return
	}
	defer cleanup()
	ctx := context.Background()
	job := &jobs.AnalysisJob{
		JobType: jobs.JobTypeBuildCodeIndex, ResourceID: "return-undispatched-mysql",
		AttemptCount: 2, MaxAttempts: 3,
	}
	if err := jobsStore.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	oldClaim, err := jobsStore.ClaimJobs(ctx, "mysql-old-worker", 1, time.Minute)
	if err != nil || len(oldClaim) != 1 || oldClaim[0].AttemptCount != 2 || oldClaim[0].ExecutionStarted {
		t.Fatalf("claim final attempt without starting: jobs=%+v err=%v", oldClaim, err)
	}
	if err := jobsStore.ReturnUndispatchedClaim(ctx, job.ID, "mysql-old-worker", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration); err != nil {
		t.Fatalf("return undispatched claim: %v", err)
	}
	returned, err := jobsStore.GetJobByID(ctx, job.ID)
	if err != nil || returned.Status != jobs.StatusPending || returned.AttemptCount != 2 || returned.WorkerID != nil || returned.ClaimToken != nil || returned.LeaseUntil != nil {
		t.Fatalf("returned job=%+v err=%v; want PENDING attempt 2 with cleared claim", returned, err)
	}
	newClaim, err := jobsStore.ClaimJobs(ctx, "mysql-new-worker", 1, time.Minute)
	if err != nil || len(newClaim) != 1 || newClaim[0].AttemptCount != 2 || newClaim[0].ExecutionStarted {
		t.Fatalf("reclaim final attempt without starting: jobs=%+v err=%v", newClaim, err)
	}
	if _, err := jobsStore.MarkExecutionStarted(ctx, job.ID, "mysql-old-worker", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale execution start error=%v, want ErrOwnershipLost", err)
	}
	if err := jobsStore.ReturnUndispatchedClaim(ctx, job.ID, "mysql-old-worker", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale claim return error=%v, want ErrOwnershipLost", err)
	}
	current, err := jobsStore.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != jobs.StatusRunning || current.AttemptCount != 2 || current.ExecutionStarted || current.WorkerID == nil || *current.WorkerID != "mysql-new-worker" || current.ClaimToken == nil || *current.ClaimToken != *newClaim[0].ClaimToken {
		t.Fatalf("stale return changed current MySQL claim: %+v", current)
	}
	if attempt, err := jobsStore.MarkExecutionStarted(ctx, job.ID, "mysql-new-worker", *newClaim[0].ClaimToken, newClaim[0].ExecutionGeneration); err != nil || attempt != 3 {
		t.Fatalf("current execution start = %d err=%v; want third attempt", attempt, err)
	}
}

type mysqlClaimGateResult struct {
	jobs []*jobs.AnalysisJob
	err  error
}

type mysqlClaimGateStore struct {
	*jobs.Store
	releaseClaims  chan struct{}
	claimed        chan mysqlClaimGateResult
	secondJobID    int64
	secondReturned chan struct{}
	secondOnce     sync.Once
}

func (s *mysqlClaimGateStore) ClaimJobs(ctx context.Context, workerID string, batchSize int, lease time.Duration) ([]*jobs.AnalysisJob, error) {
	claimed, err := s.Store.ClaimJobs(ctx, workerID, batchSize, lease)
	s.claimed <- mysqlClaimGateResult{jobs: claimed, err: err}
	<-s.releaseClaims
	return claimed, err
}

func (s *mysqlClaimGateStore) ReturnUndispatchedClaim(ctx context.Context, jobID int64, workerID, token string, generation int) error {
	err := s.Store.ReturnUndispatchedClaim(ctx, jobID, workerID, token, generation)
	if err == nil && jobID == s.secondJobID {
		s.secondOnce.Do(func() { close(s.secondReturned) })
	}
	return err
}

func TestRealMySQL_BatchUndispatchedReturnContinuesPastLockedRow(t *testing.T) {
	db, jobsStore, cleanup := setupRealMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()
	ctx := context.Background()
	first := &jobs.AnalysisJob{JobType: jobs.JobTypeRunDiagnosis, ResourceID: "mysql-batch-return-first", AttemptCount: 2, MaxAttempts: 3}
	second := &jobs.AnalysisJob{JobType: jobs.JobTypeRunDiagnosis, ResourceID: "mysql-batch-return-second", AttemptCount: 2, MaxAttempts: 3}
	for _, job := range []*jobs.AnalysisJob{first, second} {
		if err := jobsStore.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	gate := &mysqlClaimGateStore{
		Store: jobsStore, releaseClaims: make(chan struct{}), claimed: make(chan mysqlClaimGateResult, 1),
		secondJobID:    second.ID,
		secondReturned: make(chan struct{}),
	}
	cfg := jobs.DefaultWorkerConfig()
	cfg.WorkerID = "mysql-batch-shutdown-worker"
	cfg.Concurrency = 2
	cfg.BatchSize = 2
	// Keep the held row lock well within a live lease so this test exercises
	// return-loop fairness rather than lease expiry timing.
	cfg.LeaseDuration = 30 * time.Second
	cfg.PollInterval = 10 * time.Millisecond
	cfg.ReapInterval = time.Hour
	cfg.ShutdownCleanupTimeout = 10 * time.Millisecond
	worker := jobs.NewWorker(gate, cfg)
	var handlerCalls atomic.Int32
	worker.RegisterHandler(jobs.JobTypeRunDiagnosis, jobs.HandlerFunc(func(context.Context, *jobs.AnalysisJob) error {
		handlerCalls.Add(1)
		return nil
	}))
	worker.Start(ctx)
	var claimed mysqlClaimGateResult
	select {
	case claimed = <-gate.claimed:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not commit its batch claim")
	}
	if claimed.err != nil || len(claimed.jobs) != 2 || claimed.jobs[0].AttemptCount != 2 || claimed.jobs[1].AttemptCount != 2 || claimed.jobs[0].ExecutionStarted || claimed.jobs[1].ExecutionStarted {
		t.Fatalf("batch claims=%+v err=%v; want two unstarted claims at attempt 2", claimed.jobs, claimed.err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	lockTx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lockedID int64
	if err := lockTx.QueryRowContext(ctx, `SELECT id FROM analysis_jobs WHERE id = ? FOR UPDATE`, first.ID).Scan(&lockedID); err != nil {
		_ = lockTx.Rollback()
		t.Fatalf("lock first batch job: %v", err)
	}
	if lockedID != first.ID {
		_ = lockTx.Rollback()
		t.Fatalf("locked job id=%d, want %d", lockedID, first.ID)
	}

	shutdownCtx, cancelShutdown := context.WithCancel(ctx)
	cancelShutdown()
	if err := worker.StopGracefully(shutdownCtx); !errors.Is(err, context.Canceled) {
		_ = lockTx.Rollback()
		t.Fatalf("initial bounded StopGracefully=%v, want canceled cleanup result", err)
	}
	close(gate.releaseClaims)
	select {
	case <-gate.secondReturned:
	case <-time.After(10 * time.Second):
		_ = lockTx.Rollback()
		t.Fatal("second independent claim was not returned while the first row was locked")
	}
	secondState, err := jobsStore.GetJobByID(ctx, second.ID)
	if err != nil || secondState.Status != jobs.StatusPending || secondState.AttemptCount != 2 || secondState.ClaimToken != nil || secondState.LeaseUntil != nil {
		_ = lockTx.Rollback()
		t.Fatalf("second row while first locked=%+v err=%v; want safely returned attempt 2", secondState, err)
	}
	firstState, err := jobsStore.GetJobByID(ctx, first.ID)
	if err != nil || firstState.Status != jobs.StatusRunning || firstState.AttemptCount != 2 || firstState.LeaseUntil == nil || !firstState.LeaseUntil.After(time.Now().UTC()) {
		_ = lockTx.Rollback()
		t.Fatalf("locked first row state=%+v err=%v; want live claim for later return", firstState, err)
	}
	if err := lockTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	finishCtx, cancelFinish := context.WithTimeout(ctx, 10*time.Second)
	defer cancelFinish()
	if err := worker.StopGracefully(finishCtx); err != nil {
		t.Fatalf("wait for all MySQL claim returns: %v", err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		returned, err := jobsStore.GetJobByID(ctx, id)
		if err != nil || returned.Status != jobs.StatusPending || returned.AttemptCount != 2 {
			t.Fatalf("MySQL returned job %d=%+v err=%v; want PENDING attempt 2", id, returned, err)
		}
	}
	if calls := handlerCalls.Load(); calls != 0 {
		t.Fatalf("shutdown dispatched %d undispatched jobs", calls)
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

	claimed, err := jobsStore.ClaimJobs(ctx, "worker-crashing", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim failed: %v", err)
	}
	if attempt, err := jobsStore.MarkExecutionStarted(ctx, claimed[0].ID, "worker-crashing", *claimed[0].ClaimToken, claimed[0].ExecutionGeneration); err != nil || attempt != 1 {
		t.Fatalf("mark real MySQL execution started = %d err=%v", attempt, err)
	}

	// Set the persisted lease to the past using the same driver time encoding
	// as the Store. This keeps the real MySQL recovery assertion independent
	// from host/container clock skew and scheduler delays.
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE analysis_jobs SET lease_until = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second), claimed[0].ID); err != nil {
		t.Fatalf("expire started job lease: %v", err)
	}

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

func TestRealMySQL_NeverStartedExpiredClaimDoesNotExhaustUnderRowLock(t *testing.T) {
	db, jobsStore, cleanup := setupRealMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "diag-never-started-lock-reap", UserID: "user-never-started-lock-reap",
		RepositoryID: "repo-never-started-lock-reap", SnapshotID: "snap-never-started-lock-reap",
		IssueTitle: "never started under row lock", IdempotencyKey: "key-never-started-lock-reap",
		IdempotencyRequestHash: "hash-never-started-lock-reap",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Updates(map[string]interface{}{"attempt_count": 2, "max_attempts": 3}).Error; err != nil {
		t.Fatal(err)
	}
	job.AttemptCount, job.MaxAttempts = 2, 3
	lease := 650 * time.Millisecond
	claimed, err := jobsStore.ClaimJobs(ctx, "mysql-never-started-worker", 1, lease)
	if err != nil || len(claimed) != 1 || claimed[0].AttemptCount != 2 || claimed[0].ExecutionStarted {
		t.Fatalf("claim before shutdown race = %+v err=%v; want unstarted attempt 2", claimed, err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	lockTx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lockedID int64
	if err := lockTx.QueryRowContext(ctx, `SELECT id FROM analysis_jobs WHERE id = ? FOR UPDATE`, job.ID).Scan(&lockedID); err != nil {
		_ = lockTx.Rollback()
		t.Fatalf("lock never-started claim row: %v", err)
	}
	if lockedID != job.ID {
		_ = lockTx.Rollback()
		t.Fatalf("locked job ID=%d, want %d", lockedID, job.ID)
	}
	// Keep the row locked past its committed claim lease. The timer is an
	// explicit lease-boundary gate; while locked the reaper must skip the row,
	// then recover it as never-started once the lock is released. Avoid issuing
	// a timed-out write while the row is locked: a request already sent to MySQL
	// may resolve after unlock and extend the lease, making the interleaving
	// dependent on driver cancellation timing rather than the durable marker.
	timer := time.NewTimer(lease + 150*time.Millisecond)
	<-timer.C
	if reaped, err := jobsStore.ReapExpiredJobs(ctx, 10); err != nil || reaped != 0 {
		_ = lockTx.Rollback()
		t.Fatalf("reaper while row remains locked = %d err=%v; want skipped row", reaped, err)
	}
	if err := lockTx.Rollback(); err != nil {
		t.Fatal(err)
	}
	// Make expiry explicit after holding the lock beyond the lease duration.
	// The claim itself still expires naturally while locked; this write removes
	// host/container subsecond clock skew from the post-unlock reaper boundary.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE analysis_jobs SET lease_until = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second), job.ID); err != nil {
		t.Fatalf("make released claim lease explicitly expired: %v", err)
	}
	reaped, reapErr := jobsStore.ReapExpiredJobs(ctx, 10)
	if reapErr != nil || reaped != 1 {
		state, stateErr := jobsStore.GetJobByID(ctx, job.ID)
		t.Fatalf("reaper after releasing expired row = %d err=%v; state=%+v stateErr=%v, want one never-started recovery", reaped, reapErr, state, stateErr)
	}
	saved, err := jobsStore.GetJobByID(ctx, job.ID)
	if err != nil || saved.Status != jobs.StatusPending || saved.AttemptCount != 2 || saved.ExecutionStarted || saved.WorkerID != nil || saved.ClaimToken != nil {
		t.Fatalf("never-started recovery = %+v err=%v; want PENDING attempt 2", saved, err)
	}
	savedRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusQueued {
		t.Fatalf("business run after never-started recovery = %+v err=%v; want QUEUED", savedRun, err)
	}

	reclaimed, err := jobsStore.ClaimJobs(ctx, "mysql-after-expiry-worker", 1, time.Minute)
	if err != nil || len(reclaimed) != 1 || reclaimed[0].AttemptCount != 2 || reclaimed[0].ExecutionStarted {
		t.Fatalf("reclaim after expiry = %+v err=%v; want unstarted attempt 2", reclaimed, err)
	}
	if attempt, err := jobsStore.MarkExecutionStarted(ctx, job.ID, "mysql-after-expiry-worker", *reclaimed[0].ClaimToken, reclaimed[0].ExecutionGeneration); err != nil || attempt != 3 {
		t.Fatalf("start reclaimed execution = %d err=%v; want attempt 3", attempt, err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE analysis_jobs SET lease_until = DATE_SUB(UTC_TIMESTAMP(3), INTERVAL 1 SECOND) WHERE id = ?`, job.ID); err != nil {
		t.Fatal(err)
	}
	if reaped, err := jobsStore.ReapExpiredJobs(ctx, 10); err != nil || reaped != 1 {
		t.Fatalf("reaper for truly started attempt = %d err=%v; want normal exhaustion", reaped, err)
	}
	finalJob, err := jobsStore.GetJobByID(ctx, job.ID)
	finalRun, runErr := diagStore.GetByID(ctx, run.ID)
	if err != nil || runErr != nil || finalJob.Status != jobs.StatusFailed || finalJob.AttemptCount != 3 || !finalJob.ExecutionStarted || finalRun.Status != diagnosis.StatusFailed {
		t.Fatalf("started-attempt exhaustion job=%+v run=%+v errors=%v/%v", finalJob, finalRun, err, runErr)
	}
}

func TestRealMySQL_ReaperCancellationTakesPriorityOverRetryAndExhaustion(t *testing.T) {
	db, jobsStore, cleanup := setupRealMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	type reapCase struct {
		name            string
		cancelRequested bool
		maxAttempts     int
		wantJob         jobs.JobStatus
		wantRun         diagnosis.RunStatus
		wantAttempt     diagnosis.AttemptStatus
		wantReason      *jobs.TerminalReason
	}
	cancelledReason := jobs.TerminalReasonCancelled
	exhaustedReason := jobs.TerminalReasonRetryableExhausted
	cases := []reapCase{
		{name: "cancellation before exhausted lease", cancelRequested: true, maxAttempts: 1, wantJob: jobs.StatusCancelled, wantRun: diagnosis.StatusCancelled, wantAttempt: diagnosis.AttemptStatusAbandoned, wantReason: &cancelledReason},
		{name: "cancellation before retryable lease", cancelRequested: true, maxAttempts: 3, wantJob: jobs.StatusCancelled, wantRun: diagnosis.StatusCancelled, wantAttempt: diagnosis.AttemptStatusAbandoned, wantReason: &cancelledReason},
		{name: "exhausted lease without cancellation", cancelRequested: false, maxAttempts: 1, wantJob: jobs.StatusFailed, wantRun: diagnosis.StatusFailed, wantAttempt: diagnosis.AttemptStatusAbandoned, wantReason: &exhaustedReason},
		{name: "retryable lease without cancellation", cancelRequested: false, maxAttempts: 3, wantJob: jobs.StatusRetryWait, wantRun: diagnosis.StatusRunning, wantAttempt: diagnosis.AttemptStatusAbandoned},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID := fmt.Sprintf("diag-reap-cancel-%d", i)
			run := &diagnosis.DiagnosisRun{
				ID: runID, UserID: "user-" + runID, RepositoryID: "repo-reaper",
				SnapshotID: "snapshot-reaper", IssueTitle: "reaper cancellation priority",
				IdempotencyKey: "key-" + runID, IdempotencyRequestHash: "hash-" + runID,
			}
			if err := diagStore.Create(ctx, run); err != nil {
				t.Fatalf("create diagnosis: %v", err)
			}
			claimed, err := jobsStore.ClaimJobs(ctx, "worker-reaper-cancel", 1, time.Minute)
			if err != nil || len(claimed) != 1 || claimed[0].ResourceID != runID {
				t.Fatalf("claim diagnosis job: claimed=%+v err=%v", claimed, err)
			}
			attemptNo, err := jobsStore.MarkExecutionStarted(ctx, claimed[0].ID, "worker-reaper-cancel", *claimed[0].ClaimToken, claimed[0].ExecutionGeneration)
			if err != nil {
				t.Fatalf("mark diagnosis execution started: %v", err)
			}
			claimed[0].AttemptCount, claimed[0].ExecutionStarted = attemptNo, true
			attempt := &diagnosis.DiagnosisAttempt{
				ID: runID + "-attempt", DiagnosisRunID: runID, ExecutionGeneration: claimed[0].ExecutionGeneration,
				AttemptNo: claimed[0].AttemptCount, WorkerID: "worker-reaper-cancel",
			}
			if err := diagStore.StartAttempt(ctx, runID, attempt); err != nil {
				t.Fatalf("start DiagnosisAttempt: %v", err)
			}
			sqlDB, err := db.DB()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sqlDB.ExecContext(ctx, `UPDATE analysis_jobs SET max_attempts = ? WHERE id = ?`, tc.maxAttempts, claimed[0].ID); err != nil {
				t.Fatalf("set attempt limit: %v", err)
			}
			if tc.cancelRequested {
				if err := diagStore.RequestCancellation(ctx, runID, run.UserID); err != nil {
					t.Fatalf("accept user cancellation: %v", err)
				}
			}
			if _, err := sqlDB.ExecContext(ctx, `UPDATE analysis_jobs SET lease_until = ? WHERE id = ?`, time.Now().UTC().Add(-time.Second), claimed[0].ID); err != nil {
				t.Fatalf("expire job lease: %v", err)
			}

			reaped, err := jobsStore.ReapExpiredJobs(ctx, 10)
			if err != nil || reaped != 1 {
				t.Fatalf("ReapExpiredJobs = %d, err=%v; want 1", reaped, err)
			}
			savedJob, err := jobsStore.GetJobByID(ctx, claimed[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if savedJob.Status != tc.wantJob {
				t.Fatalf("Job status = %s, want %s", savedJob.Status, tc.wantJob)
			}
			if tc.wantReason == nil {
				if savedJob.TerminalReason != nil {
					t.Fatalf("Job terminal reason = %v, want nil", *savedJob.TerminalReason)
				}
			} else if savedJob.TerminalReason == nil || *savedJob.TerminalReason != *tc.wantReason {
				t.Fatalf("Job terminal reason = %v, want %s", savedJob.TerminalReason, *tc.wantReason)
			}

			savedRun, err := diagStore.GetByID(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if savedRun.Status != tc.wantRun {
				t.Fatalf("DiagnosisRun status = %s, want %s", savedRun.Status, tc.wantRun)
			}
			attempts, err := diagStore.ListAttemptsByRun(ctx, runID)
			if err != nil || len(attempts) != 1 {
				t.Fatalf("list DiagnosisAttempts: count=%d err=%v", len(attempts), err)
			}
			if attempts[0].Status != tc.wantAttempt {
				t.Fatalf("DiagnosisAttempt status = %s, want %s", attempts[0].Status, tc.wantAttempt)
			}
			if tc.wantAttempt == diagnosis.AttemptStatusAbandoned && tc.wantRun != diagnosis.StatusRunning && savedRun.FinalAttemptID != attempt.ID {
				t.Fatalf("final_attempt_id = %q, want %q", savedRun.FinalAttemptID, attempt.ID)
			}
		})
	}
}
