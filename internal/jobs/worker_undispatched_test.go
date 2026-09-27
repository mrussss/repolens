package jobs

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func newUndispatchedWorkerTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "undispatched.db?_busy_timeout=5000&_journal_mode=WAL"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
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
		)
	`)
	if err != nil {
		t.Fatal(err)
	}
	return NewStoreWithDriver(db, "sqlite3")
}

func TestWorkerReturnsClaimedUndispatchedAttemptOnGracefulShutdown(t *testing.T) {
	store := newUndispatchedWorkerTestStore(t)
	ctx := context.Background()
	job := &AnalysisJob{
		JobType: JobTypeRunDiagnosis, ResourceID: "run-shutdown-undispatched",
		AttemptCount: 2, MaxAttempts: 3,
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultWorkerConfig()
	cfg.WorkerID = "worker-before-stop"
	cfg.Concurrency = 1
	cfg.BatchSize = 1
	cfg.PollInterval = time.Millisecond
	cfg.ReapInterval = time.Hour
	worker := NewWorker(store, cfg)
	claimed := make(chan struct{})
	allowDispatchCheck := make(chan struct{})
	worker.afterClaimBeforeDispatch = func() {
		close(claimed)
		<-allowDispatchCheck
	}
	var firstExecutions atomic.Int32
	worker.RegisterHandler(JobTypeRunDiagnosis, HandlerFunc(func(context.Context, *AnalysisJob) error {
		firstExecutions.Add(1)
		return nil
	}))
	worker.Start(ctx)
	select {
	case <-claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not claim the job")
	}

	claimedJob, err := store.GetJobByID(ctx, job.ID)
	if err != nil || claimedJob.Status != StatusRunning || claimedJob.AttemptCount != 3 || claimedJob.ClaimToken == nil {
		t.Fatalf("committed claim = %+v, err=%v; want RUNNING attempt 3 with token", claimedJob, err)
	}

	stopCtx, cancelStop := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStop()
	stopped := make(chan error, 1)
	go func() { stopped <- worker.StopGracefully(stopCtx) }()
	<-worker.stopCh // StopGracefully has published shutdown before dispatch resumes.
	close(allowDispatchCheck)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("StopGracefully: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after returning its undispatched claim")
	}
	if got := firstExecutions.Load(); got != 0 {
		t.Fatalf("shutdown dispatched %d handlers, want zero", got)
	}

	returned, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if returned.Status != StatusPending || returned.AttemptCount != 2 || returned.WorkerID != nil || returned.ClaimToken != nil || returned.LeaseUntil != nil {
		t.Fatalf("undispatched claim state = %+v; want PENDING attempt 2 with cleared ownership", returned)
	}

	secondCfg := cfg
	secondCfg.WorkerID = "worker-after-stop"
	secondWorker := NewWorker(store, secondCfg)
	startedExecution := make(chan int, 1)
	var actualExecutions atomic.Int32
	secondWorker.RegisterHandler(JobTypeRunDiagnosis, HandlerFunc(func(_ context.Context, claimed *AnalysisJob) error {
		actualExecutions.Add(1)
		startedExecution <- claimed.AttemptCount
		return nil
	}))
	secondWorker.Start(ctx)
	select {
	case attempt := <-startedExecution:
		if attempt != 3 {
			t.Fatalf("actual handler attempt=%d, want third attempt", attempt)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reclaimed job handler did not start")
	}
	finishCtx, cancelFinish := context.WithTimeout(ctx, 5*time.Second)
	defer cancelFinish()
	if err := secondWorker.StopGracefully(finishCtx); err != nil {
		t.Fatalf("stop worker after actual execution: %v", err)
	}
	finished, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := actualExecutions.Load(); got != 1 || finished.Status != StatusSucceeded || finished.AttemptCount != 3 {
		t.Fatalf("actual execution count=%d, final job=%+v; want one successful third attempt", got, finished)
	}
}

func TestReturnUndispatchedClaimRejectsOldOwner(t *testing.T) {
	store := newUndispatchedWorkerTestStore(t)
	ctx := context.Background()
	job := &AnalysisJob{JobType: JobTypeRunDiagnosis, ResourceID: "run-stale-undispatched", MaxAttempts: 3}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	oldClaim, err := store.ClaimJobs(ctx, "old-worker", 1, time.Minute)
	if err != nil || len(oldClaim) != 1 {
		t.Fatalf("old claim = %d, err=%v", len(oldClaim), err)
	}
	if err := store.ReturnUndispatchedClaim(ctx, job.ID, "old-worker", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration); err != nil {
		t.Fatalf("return old claim: %v", err)
	}
	newClaim, err := store.ClaimJobs(ctx, "new-worker", 1, time.Minute)
	if err != nil || len(newClaim) != 1 {
		t.Fatalf("new claim = %d, err=%v", len(newClaim), err)
	}
	if err := store.ReturnUndispatchedClaim(ctx, job.ID, "old-worker", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("stale return error=%v, want ErrOwnershipLost", err)
	}
	current, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusRunning || current.AttemptCount != 1 || current.WorkerID == nil || *current.WorkerID != "new-worker" || current.ClaimToken == nil || *current.ClaimToken != *newClaim[0].ClaimToken {
		t.Fatalf("stale owner changed the current claim: %+v", current)
	}
}
