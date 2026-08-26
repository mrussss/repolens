package integration_real

import (
	"context"
	"errors"
	"fmt"
	"os"
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

func setupRealMySQL(t *testing.T) (*gorm.DB, *jobs.Store, func()) {
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

	if err := mysql.AutoMigrate(db); err != nil {
		_ = mysqlContainer.Terminate(ctx)
		t.Fatalf("failed to migrate real MySQL database: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		_ = mysqlContainer.Terminate(ctx)
		t.Fatalf("failed getting sql.DB: %v", err)
	}

	jobsStore := jobs.NewStore(sqlDB)

	cleanup := func() {
		_ = mysqlContainer.Terminate(context.Background())
	}

	return db, jobsStore, cleanup
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
