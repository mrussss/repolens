package jobs_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"repolens/internal/jobs"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "jobs_test.db")
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("failed opening sqlite: %v", err)
	}

	createTableQuery := `
	CREATE TABLE analysis_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		job_type TEXT NOT NULL,
		resource_id TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'PENDING',
		execution_generation INTEGER NOT NULL DEFAULT 1,
		terminal_reason TEXT,
		attempt_count INTEGER NOT NULL DEFAULT 0,
		max_attempts INTEGER NOT NULL DEFAULT 3,
		next_run_at DATETIME NOT NULL,
		worker_id TEXT,
		claim_token TEXT,
		lease_until DATETIME,
		cancel_requested BOOLEAN NOT NULL DEFAULT 0,
		last_error_class TEXT,
		last_error_code TEXT,
		last_error_message TEXT,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL,
		finished_at DATETIME,
		UNIQUE(job_type, resource_id)
	);
	CREATE INDEX ix_job_claim ON analysis_jobs(status, next_run_at, created_at, id);
	CREATE INDEX ix_job_lease ON analysis_jobs(status, lease_until);
	`
	if _, err := db.Exec(createTableQuery); err != nil {
		t.Fatalf("failed creating test table: %v", err)
	}

	return db
}

func TestErrorClassification(t *testing.T) {
	tests := []struct {
		err           error
		expectedClass jobs.ErrorClass
	}{
		{errors.New("connection reset by peer"), jobs.ErrorClassRetryable},
		{errors.New("dial tcp 127.0.0.1:80: connect: connection refused"), jobs.ErrorClassRetryable},
		{errors.New("context deadline exceeded"), jobs.ErrorClassRetryable},
		{errors.New("invalid url format"), jobs.ErrorClassPermanent},
		{errors.New("ssrf blocked link-local"), jobs.ErrorClassPermanent},
		{context.Canceled, jobs.ErrorClassCancelled},
		{jobs.ErrOwnershipLost, jobs.ErrorClassOwnershipLost},
		{jobs.NewPermanentError("BAD_REQ", "malformed json", nil), jobs.ErrorClassPermanent},
		{jobs.NewRetryableError("RATE_LIMIT", "rate limit exceeded", nil), jobs.ErrorClassRetryable},
	}

	for _, tt := range tests {
		class, _ := jobs.ClassifyError(tt.err)
		if class != tt.expectedClass {
			t.Errorf("ClassifyError(%v) = %v, want %v", tt.err, class, tt.expectedClass)
		}
	}
}

func TestHTTPStatusClassification(t *testing.T) {
	c429, _ := jobs.HTTPStatusToErrorClass(http.StatusTooManyRequests)
	if c429 != jobs.ErrorClassRetryable {
		t.Errorf("429 should be retryable")
	}

	c500, _ := jobs.HTTPStatusToErrorClass(http.StatusInternalServerError)
	if c500 != jobs.ErrorClassRetryable {
		t.Errorf("500 should be retryable")
	}

	c400, _ := jobs.HTTPStatusToErrorClass(http.StatusBadRequest)
	if c400 != jobs.ErrorClassPermanent {
		t.Errorf("400 should be permanent")
	}
}

func TestCalculateBackoff(t *testing.T) {
	b1 := jobs.CalculateBackoff(1, time.Second, time.Minute)
	if b1 < time.Second || b1 > 2*time.Second {
		t.Errorf("attempt 1 backoff unexpected: %v", b1)
	}

	b3 := jobs.CalculateBackoff(3, time.Second, time.Minute)
	if b3 < 4*time.Second || b3 > 6*time.Second {
		t.Errorf("attempt 3 backoff unexpected: %v", b3)
	}

	bMax := jobs.CalculateBackoff(10, time.Second, 10*time.Second)
	if bMax > 10*time.Second {
		t.Errorf("backoff exceeded max: %v", bMax)
	}
}

func TestStore_CreateAndClaim(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	job := &jobs.AnalysisJob{
		JobType:    jobs.JobTypeRunDiagnosis,
		ResourceID: "diag-100",
	}

	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}
	if job.ID == 0 {
		t.Fatalf("expected job ID to be set")
	}

	// Claim with worker-1
	claimed, err := store.ClaimJobs(ctx, "worker-1", 5, 10*time.Second)
	if err != nil {
		t.Fatalf("ClaimJobs failed: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("expected 1 claimed job, got %d", len(claimed))
	}
	if *claimed[0].WorkerID != "worker-1" || claimed[0].ClaimToken == nil {
		t.Fatalf("claimed job missing worker ID or claim token")
	}

	// Second claim attempt should find nothing available
	claimed2, err := store.ClaimJobs(ctx, "worker-2", 5, 10*time.Second)
	if err != nil {
		t.Fatalf("ClaimJobs second attempt failed: %v", err)
	}
	if len(claimed2) != 0 {
		t.Fatalf("expected 0 claimed jobs on second attempt, got %d", len(claimed2))
	}
}

func TestStore_LeaseRenewalAndStaleFinalize(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	job := &jobs.AnalysisJob{
		JobType:    jobs.JobTypeMaterializeSnapshot,
		ResourceID: "snap-1",
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	claimed, err := store.ClaimJobs(ctx, "worker-A", 1, 5*time.Second)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed claiming job: %v", err)
	}
	cj := claimed[0]

	// 1. Successful lease renewal
	newLease := time.Now().UTC().Add(30 * time.Second)
	if err := store.RenewLease(ctx, cj.ID, "worker-A", *cj.ClaimToken, newLease); err != nil {
		t.Fatalf("RenewLease failed: %v", err)
	}

	// 2. Lease renewal fails for stale/wrong worker
	if err := store.RenewLease(ctx, cj.ID, "worker-B", *cj.ClaimToken, newLease); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("expected ErrOwnershipLost for wrong worker, got %v", err)
	}

	// 3. Lease renewal fails for wrong claim token
	if err := store.RenewLease(ctx, cj.ID, "worker-A", "stale-token", newLease); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("expected ErrOwnershipLost for wrong token, got %v", err)
	}

	// 4. Stale finalize rejected
	if err := store.ConditionalFinalizeSuccess(ctx, cj.ID, "worker-A", "stale-token"); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("expected ErrOwnershipLost for stale finalize, got %v", err)
	}

	// 5. Authoritative finalize succeeds
	if err := store.ConditionalFinalizeSuccess(ctx, cj.ID, "worker-A", *cj.ClaimToken); err != nil {
		t.Fatalf("authoritative finalize failed: %v", err)
	}

	// Check final state
	finalJob, err := store.GetJobByID(ctx, cj.ID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if finalJob.Status != jobs.StatusSucceeded || finalJob.FinishedAt == nil {
		t.Fatalf("expected SUCCEEDED status with finished_at set")
	}
}

func TestStore_ReaperAndRetryExhaustion(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	job := &jobs.AnalysisJob{
		JobType:     jobs.JobTypeBuildCodeIndex,
		ResourceID:  "code-build-1",
		MaxAttempts: 2,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	// First claim
	claimed, err := store.ClaimJobs(ctx, "worker-1", 1, time.Millisecond)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim failed: %v", err)
	}

	// Wait for lease to expire
	time.Sleep(10 * time.Millisecond)

	// Reaper runs
	reaped, err := store.ReapExpiredJobs(ctx, 10)
	if err != nil {
		t.Fatalf("ReapExpiredJobs failed: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("expected 1 reaped job, got %d", reaped)
	}

	reapedJob, _ := store.GetJobByID(ctx, job.ID)
	if reapedJob.Status != jobs.StatusRetryWait {
		t.Fatalf("expected RETRY_WAIT after first expire, got %s", reapedJob.Status)
	}

	// Force next_run_at to past so it can be claimed again
	_, _ = db.Exec("UPDATE analysis_jobs SET next_run_at = datetime('now', '-1 minute') WHERE id = ?", job.ID)

	// Second claim (attempt 2 of 2)
	claimed2, err := store.ClaimJobs(ctx, "worker-2", 1, time.Millisecond)
	if err != nil || len(claimed2) != 1 {
		t.Fatalf("Second claim failed: %v", err)
	}
	if claimed2[0].AttemptCount != 2 {
		t.Fatalf("expected attempt count 2, got %d", claimed2[0].AttemptCount)
	}

	time.Sleep(10 * time.Millisecond)

	// Reaper runs again -> attempt_count (2) >= max_attempts (2) -> terminal FAILED
	reaped2, err := store.ReapExpiredJobs(ctx, 10)
	if err != nil || reaped2 != 1 {
		t.Fatalf("Second reap failed: %v", err)
	}

	exhaustedJob, _ := store.GetJobByID(ctx, job.ID)
	if exhaustedJob.Status != jobs.StatusFailed {
		t.Fatalf("expected FAILED status, got %s", exhaustedJob.Status)
	}
	if exhaustedJob.TerminalReason == nil || *exhaustedJob.TerminalReason != jobs.TerminalReasonRetryableExhausted {
		t.Fatalf("expected terminal reason RETRYABLE_EXHAUSTED, got %v", exhaustedJob.TerminalReason)
	}

	// Test Manual Requeue Rule
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("failed starting tx: %v", err)
	}
	err = store.ManualRequeueTx(ctx, tx, jobs.JobTypeBuildCodeIndex, "code-build-1")
	if err != nil {
		t.Fatalf("ManualRequeueTx failed: %v", err)
	}
	_ = tx.Commit()

	requeuedJob, _ := store.GetJobByID(ctx, job.ID)
	if requeuedJob.Status != jobs.StatusPending {
		t.Fatalf("expected PENDING status after manual requeue, got %s", requeuedJob.Status)
	}
	if requeuedJob.ExecutionGeneration != 2 {
		t.Fatalf("expected execution generation 2, got %d", requeuedJob.ExecutionGeneration)
	}
	if requeuedJob.AttemptCount != 0 {
		t.Fatalf("expected attempt count reset to 0, got %d", requeuedJob.AttemptCount)
	}
}

func TestWorkerRuntime_ConcurrentExecution(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	var processedCount int64

	cfg := jobs.DefaultWorkerConfig()
	cfg.Concurrency = 4
	cfg.PollInterval = 20 * time.Millisecond
	cfg.LeaseDuration = 5 * time.Second

	worker := jobs.NewWorker(store, cfg)
	worker.RegisterHandler(jobs.JobTypeRunDiagnosis, jobs.HandlerFunc(func(ctx context.Context, job *jobs.AnalysisJob) error {
		atomic.AddInt64(&processedCount, 1)
		time.Sleep(10 * time.Millisecond)
		return nil
	}))

	// Enqueue 8 jobs
	for i := 1; i <= 8; i++ {
		job := &jobs.AnalysisJob{
			JobType:    jobs.JobTypeRunDiagnosis,
			ResourceID: fmt.Sprintf("diag-batch-%d", i),
		}
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatalf("CreateJob failed: %v", err)
		}
	}

	worker.Start(ctx)

	// Wait for all 8 jobs to complete
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt64(&processedCount) < 8 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}

	worker.Stop()

	if processed := atomic.LoadInt64(&processedCount); processed != 8 {
		t.Fatalf("expected 8 processed jobs, got %d", processed)
	}
}

func TestWorkerRuntime_Cancellation(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()

	store := jobs.NewStoreWithDriver(db, "sqlite3")
	ctx := context.Background()

	cfg := jobs.DefaultWorkerConfig()
	cfg.PollInterval = 20 * time.Millisecond

	worker := jobs.NewWorker(store, cfg)

	startedCh := make(chan struct{})
	worker.RegisterHandler(jobs.JobTypeMaterializeSnapshot, jobs.HandlerFunc(func(ctx context.Context, job *jobs.AnalysisJob) error {
		close(startedCh)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
			return nil
		}
	}))

	job := &jobs.AnalysisJob{
		JobType:    jobs.JobTypeMaterializeSnapshot,
		ResourceID: "snap-cancel-1",
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("CreateJob failed: %v", err)
	}

	workerCtx, cancelWorker := context.WithCancel(ctx)
	worker.Start(workerCtx)

	// Wait for job handler to start
	<-startedCh

	// Request cancel
	cancelWorker()
	worker.Stop()

	cancelledJob, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJobByID failed: %v", err)
	}
	if cancelledJob.Status != jobs.StatusCancelled {
		t.Fatalf("expected CANCELLED status, got %s", cancelledJob.Status)
	}
}
