package jobs

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
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
			execution_started BOOLEAN NOT NULL DEFAULT 0,
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
	if err != nil || claimedJob.Status != StatusRunning || claimedJob.AttemptCount != 2 || claimedJob.ExecutionStarted || claimedJob.ClaimToken == nil {
		t.Fatalf("committed claim = %+v, err=%v; want RUNNING unstarted attempt 2 with token", claimedJob, err)
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
	if _, err := store.MarkExecutionStarted(ctx, job.ID, "old-worker", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("stale execution start error=%v, want ErrOwnershipLost", err)
	}
	if err := store.ReturnUndispatchedClaim(ctx, job.ID, "old-worker", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("stale return error=%v, want ErrOwnershipLost", err)
	}
	current, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusRunning || current.AttemptCount != 0 || current.ExecutionStarted || current.WorkerID == nil || *current.WorkerID != "new-worker" || current.ClaimToken == nil || *current.ClaimToken != *newClaim[0].ClaimToken {
		t.Fatalf("stale owner changed the current claim: %+v", current)
	}
	if attempt, err := store.MarkExecutionStarted(ctx, job.ID, "new-worker", *newClaim[0].ClaimToken, newClaim[0].ExecutionGeneration); err != nil || attempt != 1 {
		t.Fatalf("new owner execution start=%d err=%v; want attempt 1", attempt, err)
	}
}

type blockedFirstReturnStore struct {
	*Store
	firstJobID         int64
	secondJobID        int64
	firstReturnStarted chan struct{}
	allowFirstReturn   chan struct{}
	secondReturned     chan struct{}
	firstReturned      chan struct{}
	firstOnce          sync.Once
	secondOnce         sync.Once
	returnedOnce       sync.Once
}

func (s *blockedFirstReturnStore) ReturnUndispatchedClaim(ctx context.Context, jobID int64, workerID, claimToken string, generation int) error {
	if jobID == s.firstJobID {
		s.firstOnce.Do(func() { close(s.firstReturnStarted) })
		select {
		case <-s.allowFirstReturn:
		case <-ctx.Done():
			return ctx.Err()
		}
		err := s.Store.ReturnUndispatchedClaim(ctx, jobID, workerID, claimToken, generation)
		if err == nil {
			s.returnedOnce.Do(func() { close(s.firstReturned) })
		}
		return err
	}
	err := s.Store.ReturnUndispatchedClaim(ctx, jobID, workerID, claimToken, generation)
	if jobID == s.secondJobID && err == nil {
		s.secondOnce.Do(func() { close(s.secondReturned) })
	}
	return err
}

func TestWorkerReturnsBatchWithoutHeadOfLineBlockingAndProtectsPendingLease(t *testing.T) {
	store := newUndispatchedWorkerTestStore(t)
	ctx := context.Background()
	first := &AnalysisJob{JobType: JobTypeRunDiagnosis, ResourceID: "run-batch-return-first", AttemptCount: 2, MaxAttempts: 3}
	second := &AnalysisJob{JobType: JobTypeRunDiagnosis, ResourceID: "run-batch-return-second", AttemptCount: 2, MaxAttempts: 3}
	for _, job := range []*AnalysisJob{first, second} {
		if err := store.CreateJob(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	controlledStore := &blockedFirstReturnStore{
		Store: store, firstJobID: first.ID, secondJobID: second.ID,
		firstReturnStarted: make(chan struct{}), allowFirstReturn: make(chan struct{}),
		secondReturned: make(chan struct{}), firstReturned: make(chan struct{}),
	}
	cfg := DefaultWorkerConfig()
	cfg.WorkerID = "worker-batch-before-stop"
	cfg.Concurrency = 2
	cfg.BatchSize = 2
	cfg.PollInterval = 10 * time.Millisecond
	cfg.LeaseDuration = 400 * time.Millisecond
	cfg.ReapInterval = time.Hour
	worker := NewWorker(controlledStore, cfg)
	claimed := make(chan struct{})
	allowDispatchCheck := make(chan struct{})
	worker.afterClaimBeforeDispatch = func() {
		close(claimed)
		<-allowDispatchCheck
	}
	var unexpectedExecutions atomic.Int32
	worker.RegisterHandler(JobTypeRunDiagnosis, HandlerFunc(func(context.Context, *AnalysisJob) error {
		unexpectedExecutions.Add(1)
		return nil
	}))
	worker.Start(ctx)
	select {
	case <-claimed:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not claim the batch")
	}
	firstBefore, err := store.GetJobByID(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondBefore, err := store.GetJobByID(ctx, second.ID)
	if err != nil || firstBefore.Status != StatusRunning || secondBefore.Status != StatusRunning || firstBefore.AttemptCount != 2 || secondBefore.AttemptCount != 2 || firstBefore.ExecutionStarted || secondBefore.ExecutionStarted {
		t.Fatalf("batch claims first=%+v second=%+v err=%v; want both RUNNING unstarted attempt 2", firstBefore, secondBefore, err)
	}

	stopCtx, cancelStop := context.WithTimeout(ctx, 5*time.Second)
	defer cancelStop()
	stopped := make(chan error, 1)
	go func() { stopped <- worker.StopGracefully(stopCtx) }()
	<-worker.stopCh
	close(allowDispatchCheck)
	select {
	case <-controlledStore.firstReturnStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first bounded return attempt did not start")
	}
	select {
	case <-controlledStore.secondReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("second claim was not returned while first return was blocked")
	}
	firstBlocked, err := store.GetJobByID(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	secondReturned, err := store.GetJobByID(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if firstBlocked.Status != StatusRunning || firstBlocked.AttemptCount != 2 || firstBlocked.LeaseUntil == nil || firstBefore.LeaseUntil == nil || !firstBlocked.LeaseUntil.After(*firstBefore.LeaseUntil) {
		t.Fatalf("blocked first claim was not lease-protected: before=%+v after=%+v", firstBefore, firstBlocked)
	}
	if secondReturned.Status != StatusPending || secondReturned.AttemptCount != 2 || secondReturned.WorkerID != nil || secondReturned.ClaimToken != nil || secondReturned.LeaseUntil != nil {
		t.Fatalf("second claim state=%+v; want returned PENDING attempt 2", secondReturned)
	}
	close(controlledStore.allowFirstReturn)
	select {
	case <-controlledStore.firstReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("first claim was not returned after releasing its barrier")
	}
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatalf("StopGracefully: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop after returning the full batch")
	}
	for _, id := range []int64{first.ID, second.ID} {
		returned, err := store.GetJobByID(ctx, id)
		if err != nil || returned.Status != StatusPending || returned.AttemptCount != 2 {
			t.Fatalf("returned job %d = %+v err=%v; want PENDING attempt 2", id, returned, err)
		}
	}
	if got := unexpectedExecutions.Load(); got != 0 {
		t.Fatalf("shutdown started %d handlers for undispatched claims", got)
	}

	secondCfg := cfg
	secondCfg.WorkerID = "worker-batch-after-stop"
	secondWorker := NewWorker(store, secondCfg)
	actualRuns := make(chan int, 2)
	secondWorker.RegisterHandler(JobTypeRunDiagnosis, HandlerFunc(func(_ context.Context, job *AnalysisJob) error {
		actualRuns <- job.AttemptCount
		return nil
	}))
	secondWorker.Start(ctx)
	for i := 0; i < 2; i++ {
		select {
		case attempt := <-actualRuns:
			if attempt != 3 {
				t.Fatalf("subsequent handler attempt=%d, want 3", attempt)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("later worker did not execute both returned jobs")
		}
	}
	finishCtx, cancelFinish := context.WithTimeout(ctx, 5*time.Second)
	defer cancelFinish()
	if err := secondWorker.StopGracefully(finishCtx); err != nil {
		t.Fatalf("stop worker after actual batch execution: %v", err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		finished, err := store.GetJobByID(ctx, id)
		if err != nil || finished.Status != StatusSucceeded || finished.AttemptCount != 3 {
			t.Fatalf("executed job %d = %+v err=%v; want SUCCEEDED attempt 3", id, finished, err)
		}
	}
}
