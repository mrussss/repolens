package jobs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var errInjectedFinalize = errors.New("injected transient finalization database error")

type finalizationFaultStore struct {
	*Store

	successCalls atomic.Int32
	failureCalls atomic.Int32
	cancelCalls  atomic.Int32
	resolveReads atomic.Int32

	transientSuccessOnce bool
	ambiguousSuccessOnce bool
	loseSuccessOwnership bool
	blockSuccessCall     int32
	transientFailureOnce bool
	transientCancelOnce  bool
	alwaysFailSuccess    bool

	firstSuccess  sync.Once
	secondSuccess sync.Once
	secondFailure sync.Once
	secondCancel  sync.Once
	leaseRenewed  sync.Once

	firstSuccessCall   chan struct{}
	secondSuccessCall  chan struct{}
	allowSecondSuccess chan struct{}
	leaseRenewedCh     chan struct{}
	failureDone        chan struct{}
	cancelDone         chan struct{}
	finalizing         atomic.Bool
}

func newFinalizationFaultStore(store *Store) *finalizationFaultStore {
	return &finalizationFaultStore{
		Store:              store,
		firstSuccessCall:   make(chan struct{}),
		secondSuccessCall:  make(chan struct{}),
		allowSecondSuccess: make(chan struct{}),
		leaseRenewedCh:     make(chan struct{}),
		failureDone:        make(chan struct{}),
		cancelDone:         make(chan struct{}),
	}
}

func (s *finalizationFaultStore) GetJobByID(ctx context.Context, id int64) (*AnalysisJob, error) {
	s.resolveReads.Add(1)
	return s.Store.GetJobByID(ctx, id)
}

func (s *finalizationFaultStore) RenewLease(ctx context.Context, id int64, workerID, token string, until time.Time) error {
	err := s.Store.RenewLease(ctx, id, workerID, token, until)
	if err == nil && s.finalizing.Load() {
		s.leaseRenewed.Do(func() { close(s.leaseRenewedCh) })
	}
	return err
}

func (s *finalizationFaultStore) ConditionalFinalizeSuccess(ctx context.Context, id int64, workerID, token string) error {
	call := s.successCalls.Add(1)
	if call == 1 {
		s.firstSuccess.Do(func() { close(s.firstSuccessCall) })
		if s.alwaysFailSuccess || s.transientSuccessOnce || s.loseSuccessOwnership {
			if s.loseSuccessOwnership {
				_, err := s.Store.db.ExecContext(ctx, `UPDATE analysis_jobs
					SET worker_id = 'replacement-worker', claim_token = 'replacement-token',
					    execution_generation = execution_generation + 1,
					    lease_until = datetime('now', '+1 minute')
					WHERE id = ?`, id)
				if err != nil {
					return err
				}
			}
			return errInjectedFinalize
		}
		if s.ambiguousSuccessOnce {
			err := s.Store.ConditionalFinalizeSuccess(ctx, id, workerID, token)
			if err != nil {
				return err
			}
			return errInjectedFinalize
		}
	}
	if call == s.blockSuccessCall {
		s.finalizing.Store(true)
		s.secondSuccess.Do(func() { close(s.secondSuccessCall) })
		select {
		case <-s.allowSecondSuccess:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if s.alwaysFailSuccess {
		return errInjectedFinalize
	}
	err := s.Store.ConditionalFinalizeSuccess(ctx, id, workerID, token)
	return err
}

func (s *finalizationFaultStore) ConditionalFinalizeFailure(ctx context.Context, id int64, workerID, token string, class ErrorClass, code, message string, reason *TerminalReason, terminal bool, nextRun time.Time) error {
	call := s.failureCalls.Add(1)
	if call == 1 && s.transientFailureOnce {
		return errInjectedFinalize
	}
	err := s.Store.ConditionalFinalizeFailure(ctx, id, workerID, token, class, code, message, reason, terminal, nextRun)
	if err == nil {
		s.secondFailure.Do(func() { close(s.failureDone) })
	}
	return err
}

func (s *finalizationFaultStore) ConditionalFinalizeCancel(ctx context.Context, id int64, workerID, token string) error {
	call := s.cancelCalls.Add(1)
	if call == 1 && s.transientCancelOnce {
		return errInjectedFinalize
	}
	err := s.Store.ConditionalFinalizeCancel(ctx, id, workerID, token)
	if err == nil {
		s.secondCancel.Do(func() { close(s.cancelDone) })
	}
	return err
}

func startFinalizationTestWorker(t *testing.T, store workerStore, workerID string, lease time.Duration, handler Handler) *Worker {
	t.Helper()
	cfg := DefaultWorkerConfig()
	cfg.WorkerID = workerID
	cfg.Concurrency = 1
	cfg.BatchSize = 1
	cfg.PollInterval = 5 * time.Millisecond
	cfg.LeaseDuration = lease
	cfg.ReapInterval = time.Hour
	worker := NewWorker(store, cfg)
	worker.RegisterHandler(JobTypeRunDiagnosis, handler)
	worker.Start(context.Background())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = worker.StopGracefully(ctx)
	})
	return worker
}

func stopFinalizationTestWorker(t *testing.T, worker *Worker) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return worker.StopGracefully(ctx)
}

func TestWorkerRetriesTransientSuccessFinalizationAndDrainsBeforeReturning(t *testing.T) {
	base := newUndispatchedWorkerTestStore(t)
	store := newFinalizationFaultStore(base)
	store.transientSuccessOnce = true
	store.blockSuccessCall = 2
	job := &AnalysisJob{JobType: JobTypeRunDiagnosis, ResourceID: "success-finalization-transient", MaxAttempts: 3}
	if err := base.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	handlerReturned := make(chan struct{})
	worker := startFinalizationTestWorker(t, store, "success-transient-worker", 450*time.Millisecond, HandlerFunc(func(context.Context, *AnalysisJob) error {
		close(handlerReturned)
		return nil
	}))
	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
	select {
	case <-store.firstSuccessCall:
	case <-time.After(5 * time.Second):
		t.Fatal("first success finalization was not attempted")
	}

	stopped := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		stopped <- worker.StopGracefully(ctx)
	}()
	select {
	case <-store.secondSuccessCall:
	case err := <-stopped:
		t.Fatalf("worker stopped before retrying finalization: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("success finalization was not retried")
	}
	select {
	case <-stopped:
		t.Fatal("StopGracefully returned while success finalization was unresolved")
	default:
	}
	jobState, err := base.GetJobByID(context.Background(), job.ID)
	if err != nil || jobState.Status != StatusRunning || !jobState.ExecutionStarted || jobState.LeaseUntil == nil {
		t.Fatalf("job during blocked finalization=%+v err=%v; want RUNNING started claim", jobState, err)
	}
	leaseBeforeRenew := *jobState.LeaseUntil
	select {
	case <-store.leaseRenewedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("finalization did not keep renewing its claim lease")
	}
	jobState, err = base.GetJobByID(context.Background(), job.ID)
	if err != nil || jobState.LeaseUntil == nil || !jobState.LeaseUntil.After(leaseBeforeRenew) {
		t.Fatalf("lease during pending finalization=%+v err=%v; want it renewed", jobState, err)
	}
	close(store.allowSecondSuccess)
	if err := <-stopped; err != nil {
		t.Fatalf("StopGracefully after resolving success: %v", err)
	}
	finished, err := base.GetJobByID(context.Background(), job.ID)
	if err != nil || finished.Status != StatusSucceeded || finished.AttemptCount != 1 {
		t.Fatalf("final job=%+v err=%v; want SUCCEEDED attempt 1", finished, err)
	}
}

func TestWorkerResolvesAmbiguousSuccessCommitForSameClaim(t *testing.T) {
	base := newUndispatchedWorkerTestStore(t)
	store := newFinalizationFaultStore(base)
	store.ambiguousSuccessOnce = true
	job := &AnalysisJob{JobType: JobTypeRunDiagnosis, ResourceID: "success-finalization-ambiguous", MaxAttempts: 3}
	if err := base.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	handlerReturned := make(chan struct{})
	worker := startFinalizationTestWorker(t, store, "success-ambiguous-worker", time.Second, HandlerFunc(func(context.Context, *AnalysisJob) error {
		close(handlerReturned)
		return nil
	}))
	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
	if err := stopFinalizationTestWorker(t, worker); err != nil {
		t.Fatalf("StopGracefully: %v", err)
	}
	finished, err := base.GetJobByID(context.Background(), job.ID)
	if err != nil || finished.Status != StatusSucceeded || finished.AttemptCount != 1 {
		t.Fatalf("ambiguous committed job=%+v err=%v; want SUCCEEDED attempt 1", finished, err)
	}
	if got := store.successCalls.Load(); got != 1 {
		t.Fatalf("success finalizer calls=%d; want one because same-claim commit was resolved", got)
	}
	if store.resolveReads.Load() == 0 {
		t.Fatal("worker did not read durable state to resolve the ambiguous commit")
	}
}

func TestWorkerStopsFinalizationWhenOwnershipMovesToNewClaim(t *testing.T) {
	base := newUndispatchedWorkerTestStore(t)
	store := newFinalizationFaultStore(base)
	store.loseSuccessOwnership = true
	job := &AnalysisJob{JobType: JobTypeRunDiagnosis, ResourceID: "success-finalization-lost-owner", MaxAttempts: 3}
	if err := base.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	handlerReturned := make(chan struct{})
	worker := startFinalizationTestWorker(t, store, "success-losing-worker", time.Second, HandlerFunc(func(context.Context, *AnalysisJob) error {
		close(handlerReturned)
		return nil
	}))
	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return")
	}
	if err := stopFinalizationTestWorker(t, worker); err != nil {
		t.Fatalf("StopGracefully after confirmed ownership loss: %v", err)
	}
	current, err := base.GetJobByID(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != StatusRunning || current.WorkerID == nil || *current.WorkerID != "replacement-worker" || current.ClaimToken == nil || *current.ClaimToken != "replacement-token" || current.ExecutionGeneration != 2 {
		t.Fatalf("replacement claim changed by stale finalizer: %+v", current)
	}
	if got := store.successCalls.Load(); got != 1 {
		t.Fatalf("stale finalizer calls=%d; want no retries after ownership was lost", got)
	}
}

func TestWorkerRetriesFailureAndCancelFinalizationErrors(t *testing.T) {
	t.Run("failure", func(t *testing.T) {
		base := newUndispatchedWorkerTestStore(t)
		store := newFinalizationFaultStore(base)
		store.transientFailureOnce = true
		job := &AnalysisJob{JobType: JobTypeRunDiagnosis, ResourceID: "failure-finalization-transient", MaxAttempts: 3}
		if err := base.CreateJob(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		worker := startFinalizationTestWorker(t, store, "failure-transient-worker", time.Second, HandlerFunc(func(context.Context, *AnalysisJob) error {
			return NewRetryableError("HANDLER_RETRY", "retry after handler failure", nil)
		}))
		select {
		case <-store.failureDone:
		case <-time.After(5 * time.Second):
			t.Fatal("failure finalization did not retry and complete")
		}
		if err := stopFinalizationTestWorker(t, worker); err != nil {
			t.Fatalf("StopGracefully: %v", err)
		}
		final, err := base.GetJobByID(context.Background(), job.ID)
		if err != nil || final.Status != StatusRetryWait || final.AttemptCount != 1 || store.failureCalls.Load() != 2 {
			t.Fatalf("failure finalization state=%+v calls=%d err=%v; want RETRY_WAIT after retry", final, store.failureCalls.Load(), err)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		base := newUndispatchedWorkerTestStore(t)
		store := newFinalizationFaultStore(base)
		store.transientCancelOnce = true
		job := &AnalysisJob{JobType: JobTypeRunDiagnosis, ResourceID: "cancel-finalization-transient", MaxAttempts: 3}
		if err := base.CreateJob(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		worker := startFinalizationTestWorker(t, store, "cancel-transient-worker", time.Second, HandlerFunc(func(ctx context.Context, claimed *AnalysisJob) error {
			if err := base.RequestCancel(ctx, claimed.JobType, claimed.ResourceID); err != nil {
				return err
			}
			return ErrUserCancellation
		}))
		select {
		case <-store.cancelDone:
		case <-time.After(5 * time.Second):
			t.Fatal("cancel finalization did not retry and complete")
		}
		if err := stopFinalizationTestWorker(t, worker); err != nil {
			t.Fatalf("StopGracefully: %v", err)
		}
		final, err := base.GetJobByID(context.Background(), job.ID)
		if err != nil || final.Status != StatusCancelled || !final.CancelRequested || store.cancelCalls.Load() != 2 {
			t.Fatalf("cancel finalization state=%+v calls=%d err=%v; want CANCELLED after retry", final, store.cancelCalls.Load(), err)
		}
	})
}

func TestWorkerStopReturnsErrorWhenFinalizationCannotResolve(t *testing.T) {
	base := newUndispatchedWorkerTestStore(t)
	store := newFinalizationFaultStore(base)
	store.alwaysFailSuccess = true
	job := &AnalysisJob{JobType: JobTypeRunDiagnosis, ResourceID: "success-finalization-unresolved", MaxAttempts: 3}
	if err := base.CreateJob(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	worker := startFinalizationTestWorker(t, store, "success-unresolved-worker", 2*time.Second, HandlerFunc(func(context.Context, *AnalysisJob) error { return nil }))
	select {
	case <-store.firstSuccessCall:
	case <-time.After(5 * time.Second):
		t.Fatal("success finalization was not attempted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := worker.StopGracefully(ctx); err == nil {
		t.Fatal("StopGracefully returned nil with unresolved finalization")
	}
	jobState, err := base.GetJobByID(context.Background(), job.ID)
	if err != nil || jobState.Status != StatusRunning || !jobState.ExecutionStarted {
		t.Fatalf("unresolved finalization job=%+v err=%v; want RUNNING started claim", jobState, err)
	}
	if store.successCalls.Load() < 2 {
		t.Fatalf("success finalizer calls=%d; want retry before shutdown timeout", store.successCalls.Load())
	}
}
