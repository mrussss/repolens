package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"repolens/internal/platform/logger"
)

// Handler processes a single claimed AnalysisJob.
type Handler interface {
	Execute(ctx context.Context, job *AnalysisJob) error
}

// HandlerFunc is an adapter allowing a function to act as a Handler.
type HandlerFunc func(ctx context.Context, job *AnalysisJob) error

func (f HandlerFunc) Execute(ctx context.Context, job *AnalysisJob) error {
	return f(ctx, job)
}

// WorkerConfig holds configuration for the job worker runtime.
type WorkerConfig struct {
	WorkerID               string
	Concurrency            int
	BatchSize              int
	PollInterval           time.Duration
	LeaseDuration          time.Duration
	ReapInterval           time.Duration
	BaseBackoff            time.Duration
	MaxBackoff             time.Duration
	ShutdownCleanupTimeout time.Duration
}

// DefaultWorkerConfig returns production defaults for the worker.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		WorkerID:               "worker-" + uuid.New().String()[:8],
		Concurrency:            4,
		BatchSize:              4,
		PollInterval:           time.Second,
		LeaseDuration:          30 * time.Second,
		ReapInterval:           10 * time.Second,
		BaseBackoff:            2 * time.Second,
		MaxBackoff:             60 * time.Second,
		ShutdownCleanupTimeout: 5 * time.Second,
	}
}

type workerStore interface {
	ClaimJobs(context.Context, string, int, time.Duration) ([]*AnalysisJob, error)
	GetJobByID(context.Context, int64) (*AnalysisJob, error)
	MarkExecutionStarted(context.Context, int64, string, string, int) (int, error)
	ReturnUndispatchedClaim(context.Context, int64, string, string, int) error
	RenewLease(context.Context, int64, string, string, time.Time) error
	IsCancelRequested(context.Context, int64, string, string) (bool, error)
	ConditionalFinalizeSuccess(context.Context, int64, string, string) error
	ConditionalFinalizeFailure(context.Context, int64, string, string, ErrorClass, string, string, *TerminalReason, bool, time.Time) error
	ConditionalFinalizeCancel(context.Context, int64, string, string) error
	ReapExpiredJobs(context.Context, int) (int, error)
}

// Worker executes async jobs claimed from the store.
type Worker struct {
	store                    workerStore
	cfg                      WorkerConfig
	handlers                 map[JobType]Handler
	mu                       sync.RWMutex
	dispatchMu               sync.Mutex
	dispatchStopped          bool
	afterClaimBeforeDispatch func()
	loopWG                   sync.WaitGroup
	jobWG                    sync.WaitGroup
	stopCh                   chan struct{}
	stopOnce                 sync.Once
	activeMu                 sync.Mutex
	active                   map[int64]context.CancelCauseFunc
	stopping                 bool
	finalizationMu           sync.Mutex
	finalizationErrors       map[int64]error
}

// NewWorker constructs a new Worker.
func NewWorker(store workerStore, cfg WorkerConfig) *Worker {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = cfg.Concurrency
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 30 * time.Second
	}
	if cfg.ReapInterval <= 0 {
		cfg.ReapInterval = 10 * time.Second
	}
	if cfg.ShutdownCleanupTimeout <= 0 {
		cfg.ShutdownCleanupTimeout = 5 * time.Second
	}

	return &Worker{
		store:              store,
		cfg:                cfg,
		handlers:           make(map[JobType]Handler),
		stopCh:             make(chan struct{}),
		active:             make(map[int64]context.CancelCauseFunc),
		finalizationErrors: make(map[int64]error),
	}
}

// RegisterHandler registers a Handler for a specific JobType.
func (w *Worker) RegisterHandler(jobType JobType, handler Handler) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handlers[jobType] = handler
}

// Start launches the worker claim loop and reaper in background goroutines.
func (w *Worker) Start(ctx context.Context) {
	log := logger.L(ctx)
	log.Info("starting analysis jobs worker", "worker_id", w.cfg.WorkerID, "concurrency", w.cfg.Concurrency)

	w.loopWG.Add(2)
	go w.claimLoop(ctx)
	go w.reapLoop(ctx)
}

// Stop waits for in-flight jobs for a bounded period before asking remaining
// handlers to stop as worker shutdown.
func (w *Worker) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = w.StopGracefully(ctx)
}

// StopGracefully stops new claims, drains active jobs until ctx expires, then
// cancels remaining handlers with ErrWorkerShutdown and waits for bounded
// cleanup. Jobs that do not finish remain RUNNING for lease recovery.
func (w *Worker) StopGracefully(ctx context.Context) error {
	w.dispatchMu.Lock()
	w.dispatchStopped = true
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.dispatchMu.Unlock()
	loopsDone := waitGroupDone(&w.loopWG)
	if err := waitForContext(ctx, loopsDone); err != nil {
		w.cancelActive(ErrWorkerShutdown)
		return w.waitForShutdownCleanup(loopsDone, err)
	}

	jobsDone := waitGroupDone(&w.jobWG)
	if err := waitForContext(ctx, jobsDone); err != nil {
		w.cancelActive(ErrWorkerShutdown)
		return w.waitForShutdownCleanup(jobsDone, err)
	}
	if err := w.unresolvedFinalizationError(); err != nil {
		return err
	}
	return nil
}

func waitGroupDone(wg *sync.WaitGroup) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done
}

func waitForContext(ctx context.Context, done <-chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Worker) waitForShutdownCleanup(done <-chan struct{}, cause error) error {
	timer := time.NewTimer(w.cfg.ShutdownCleanupTimeout)
	defer timer.Stop()
	select {
	case <-done:
		return cause
	case <-timer.C:
		return fmt.Errorf("worker shutdown cleanup exceeded %s: %w", w.cfg.ShutdownCleanupTimeout, cause)
	}
}

func (w *Worker) registerActive(jobID int64, cancel context.CancelCauseFunc) {
	w.activeMu.Lock()
	defer w.activeMu.Unlock()
	if w.stopping {
		cancel(ErrWorkerShutdown)
		return
	}
	w.active[jobID] = cancel
}

func (w *Worker) replaceActive(jobID int64, cancel context.CancelCauseFunc) {
	w.activeMu.Lock()
	if w.stopping {
		w.activeMu.Unlock()
		cancel(ErrWorkerShutdown)
		return
	}
	w.active[jobID] = cancel
	w.activeMu.Unlock()
}

func (w *Worker) unregisterActive(jobID int64) {
	w.activeMu.Lock()
	delete(w.active, jobID)
	w.activeMu.Unlock()
}

func (w *Worker) cancelActive(cause error) {
	w.activeMu.Lock()
	w.stopping = true
	cancels := make([]context.CancelCauseFunc, 0, len(w.active))
	for _, cancel := range w.active {
		cancels = append(cancels, cancel)
	}
	w.activeMu.Unlock()
	for _, cancel := range cancels {
		cancel(cause)
	}
}

func (w *Worker) recordFinalizationError(jobID int64, err error) {
	w.finalizationMu.Lock()
	w.finalizationErrors[jobID] = err
	w.finalizationMu.Unlock()
}

func (w *Worker) clearFinalizationError(jobID int64) {
	w.finalizationMu.Lock()
	delete(w.finalizationErrors, jobID)
	w.finalizationMu.Unlock()
}

func (w *Worker) unresolvedFinalizationError() error {
	w.finalizationMu.Lock()
	defer w.finalizationMu.Unlock()
	for jobID, err := range w.finalizationErrors {
		return fmt.Errorf("job %d has unresolved terminal finalization: %w", jobID, err)
	}
	return nil
}

func (w *Worker) claimLoop(ctx context.Context) {
	defer w.loopWG.Done()
	sem := make(chan struct{}, w.cfg.Concurrency)

	for {
		select {
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		// Reserve execution capacity before claiming. ClaimJobs starts each
		// lease immediately, so a job must never wait for a semaphore after it
		// has become RUNNING. BatchSize remains an upper bound, while free
		// concurrency slots determine the actual claim size.
		reserved := 0
	reserveSlots:
		for reserved < w.cfg.BatchSize {
			select {
			case sem <- struct{}{}:
				reserved++
			default:
				break reserveSlots
			}
		}
		if reserved == 0 {
			select {
			case <-w.stopCh:
				return
			case <-ctx.Done():
				return
			case <-time.After(w.cfg.PollInterval):
				continue
			}
		}

		jobs, err := w.store.ClaimJobs(ctx, w.cfg.WorkerID, reserved, w.cfg.LeaseDuration)
		if err != nil {
			for i := 0; i < reserved; i++ {
				<-sem
			}
			log := logger.L(ctx)
			log.Error("error claiming jobs", "worker_id", w.cfg.WorkerID, "error", err)
			select {
			case <-w.stopCh:
				return
			case <-ctx.Done():
				return
			case <-time.After(w.cfg.PollInterval):
			}
			continue
		}
		for i := len(jobs); i < reserved; i++ {
			<-sem
		}
		if len(jobs) == 0 {
			select {
			case <-w.stopCh:
				return
			case <-ctx.Done():
				return
			case <-time.After(w.cfg.PollInterval):
				continue
			}
		}

		if w.afterClaimBeforeDispatch != nil {
			w.afterClaimBeforeDispatch()
		}
		for i, job := range jobs {
			w.dispatchMu.Lock()
			if w.dispatchStopped || ctx.Err() != nil {
				w.dispatchMu.Unlock()
				w.returnUndispatchedClaims(ctx, sem, jobs[i:])
				return
			}
			attemptCount, err := w.startClaimedExecution(job)
			if err != nil {
				w.dispatchMu.Unlock()
				if errors.Is(err, ErrOwnershipLost) {
					<-sem
					continue
				}
				logger.L(ctx).Error("unable to resolve execution-start transition", "job_id", job.ID, "worker_id", w.cfg.WorkerID, "error", err)
				// startClaimedExecution only returns non-ownership errors when its
				// retry context is cancelled; leave the claim for lease recovery.
				<-sem
				continue
			}
			job.AttemptCount = attemptCount
			job.ExecutionStarted = true
			w.jobWG.Add(1)
			go func(j *AnalysisJob) {
				defer func() {
					<-sem
					w.jobWG.Done()
				}()
				w.executeJob(ctx, j)
			}(job)
			w.dispatchMu.Unlock()
		}
	}
}

// startClaimedExecution commits the attempt charge before a handler can be
// launched. It retries ambiguous/transient store errors because the durable
// transition is idempotent for the same live claim.
func (w *Worker) startClaimedExecution(job *AnalysisJob) (int, error) {
	for {
		operationTimeout := w.undispatchedClaimOperationTimeout()
		ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
		attemptCount, err := w.store.MarkExecutionStarted(ctx, job.ID, w.cfg.WorkerID, w.undispatchedClaimToken(job), job.ExecutionGeneration)
		cancel()
		if err == nil || errors.Is(err, ErrOwnershipLost) {
			return attemptCount, err
		}
		logger.L(context.Background()).Error("error marking execution started; retrying claim-fenced transition", "job_id", job.ID, "worker_id", w.cfg.WorkerID, "error", err)
		if renewErr := w.renewUndispatchedClaimLease(job, operationTimeout); renewErr != nil && !errors.Is(renewErr, ErrOwnershipLost) {
			logger.L(context.Background()).Warn("error renewing claim while resolving execution-start transition", "job_id", job.ID, "worker_id", w.cfg.WorkerID, "error", renewErr)
		}
		timer := time.NewTimer(w.cfg.PollInterval)
		<-timer.C
	}
}

func (w *Worker) returnUndispatchedClaims(parentCtx context.Context, sem chan struct{}, jobs []*AnalysisJob) {
	pending := append([]*AnalysisJob(nil), jobs...)
	operationTimeout := w.undispatchedClaimOperationTimeout()
	retryDelay := w.cfg.PollInterval
	if retryDelay > operationTimeout {
		retryDelay = operationTimeout
	}

	for len(pending) > 0 {
		stillOwned := pending[:0]
		for _, job := range pending {
			err := w.renewUndispatchedClaimLease(job, operationTimeout)
			if err != nil {
				logger.L(parentCtx).Error("error renewing undispatched job lease; return will verify claim ownership", "job_id", job.ID, "worker_id", w.cfg.WorkerID, "error", err)
			}
			stillOwned = append(stillOwned, job)
		}
		pending = stillOwned

		stillPending := pending[:0]
		for _, job := range pending {
			err := w.returnUndispatchedClaim(job, operationTimeout)
			if err == nil || errors.Is(err, ErrOwnershipLost) {
				<-sem
				continue
			}
			logger.L(parentCtx).Error("error returning undispatched job claim; will retry after processing the batch", "job_id", job.ID, "worker_id", w.cfg.WorkerID, "error", err)
			stillPending = append(stillPending, job)
		}
		pending = stillPending
		if len(pending) > 0 {
			timer := time.NewTimer(retryDelay)
			<-timer.C
		}
	}
}

func (w *Worker) undispatchedClaimOperationTimeout() time.Duration {
	timeout := w.cfg.LeaseDuration / 4
	if timeout <= 0 {
		timeout = w.cfg.LeaseDuration
	}
	if timeout > time.Second {
		timeout = time.Second
	}
	return timeout
}

func (w *Worker) undispatchedClaimToken(job *AnalysisJob) string {
	claimToken := ""
	if job.ClaimToken != nil {
		claimToken = *job.ClaimToken
	}
	return claimToken
}

func (w *Worker) renewUndispatchedClaimLease(job *AnalysisJob, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return w.store.RenewLease(ctx, job.ID, w.cfg.WorkerID, w.undispatchedClaimToken(job), time.Now().UTC().Add(w.cfg.LeaseDuration))
}

func (w *Worker) returnUndispatchedClaim(job *AnalysisJob, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return w.store.ReturnUndispatchedClaim(ctx, job.ID, w.cfg.WorkerID, w.undispatchedClaimToken(job), job.ExecutionGeneration)
}

func (w *Worker) executeJob(parentCtx context.Context, job *AnalysisJob) {
	log := logger.L(parentCtx).With(
		"job_id", job.ID,
		"job_type", string(job.JobType),
		"resource_id", job.ResourceID,
		"attempt", job.AttemptCount,
		"worker_id", w.cfg.WorkerID,
	)

	jobCtx, cancelJob := context.WithCancelCause(parentCtx)
	w.registerActive(job.ID, cancelJob)
	defer w.unregisterActive(job.ID)
	defer cancelJob(context.Canceled)

	// Start background lease renewer
	renewer := StartLeaseRenewer(jobCtx, w.store, job.ID, w.cfg.WorkerID, *job.ClaimToken, w.cfg.LeaseDuration, cancelJob)
	defer renewer.Stop()
	cancelPollStop := make(chan struct{})
	cancelPollDone := make(chan struct{})
	go func() {
		defer close(cancelPollDone)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-cancelPollStop:
				return
			case <-ticker.C:
				requested, err := w.store.IsCancelRequested(jobCtx, job.ID, w.cfg.WorkerID, *job.ClaimToken)
				if err != nil {
					if errors.Is(err, ErrOwnershipLost) {
						cancelJob(ErrOwnershipLost)
					} else {
						cancelJob(fmt.Errorf("%w: %w", ErrCancelPollFailed, err))
					}
					return
				}
				if requested {
					cancelJob(ErrUserCancellation)
					return
				}
			}
		}
	}()
	stopCancelPoll := func() {
		select {
		case <-cancelPollStop:
		default:
			close(cancelPollStop)
		}
		<-cancelPollDone
	}
	finish := func(finalize func(context.Context) error, expected func(*AnalysisJob) bool) {
		finalCtx, cancelFinal := context.WithCancelCause(context.Background())
		w.replaceActive(job.ID, cancelFinal)
		stopFinalLeaseRenewal := w.startFinalizationLeaseRenewer(finalCtx, job)
		stopCancelPoll()
		cancelJob(context.Canceled)
		renewer.Stop()

		if err := w.resolveFinalization(finalCtx, job, finalize, expected); err != nil {
			if !errors.Is(err, ErrOwnershipLost) {
				w.recordFinalizationError(job.ID, err)
				log.Error("terminal job finalization remains unresolved", "error", err)
			} else {
				w.clearFinalizationError(job.ID)
				log.Warn("terminal job finalization stopped after ownership loss", "error", err)
			}
		} else {
			w.clearFinalizationError(job.ID)
		}
		stopFinalLeaseRenewal()
		cancelFinal(context.Canceled)
	}

	w.mu.RLock()
	handler, exists := w.handlers[job.JobType]
	w.mu.RUnlock()
	if !exists {
		log.Error("no handler registered for job type", "job_type", job.JobType)
		termReason := TerminalReasonPermanent
		code := "NO_HANDLER_REGISTERED"
		message := fmt.Sprintf("no handler registered for job type %s", job.JobType)
		finish(func(ctx context.Context) error {
			return w.store.ConditionalFinalizeFailure(ctx, job.ID, w.cfg.WorkerID, *job.ClaimToken,
				ErrorClassPermanent, code, message, &termReason, true, time.Time{})
		}, failureFinalizationExpected(ErrorClassPermanent, code, &termReason, true))
		return
	}

	// Check if cancellation was requested before execution
	if job.CancelRequested {
		log.Info("job was cancel_requested prior to execution")
		cancelJob(ErrUserCancellation)
		finish(func(ctx context.Context) error {
			return w.store.ConditionalFinalizeCancel(ctx, job.ID, w.cfg.WorkerID, *job.ClaimToken)
		}, cancelledFinalizationExpected)
		return
	}

	start := time.Now()
	err := handler.Execute(jobCtx, job)
	stopCancelPoll()
	cause := context.Cause(jobCtx)
	if errors.Is(cause, ErrWorkerShutdown) {
		cancelJob(context.Canceled)
		renewer.Stop()
		log.Info("job execution stopped for worker shutdown; lease recovery will resume it")
		return
	}
	if cause != nil {
		err = cause
	}
	latency := time.Since(start)

	if err == nil {
		log.Info("job execution succeeded", "latency_ms", latency.Milliseconds())
		finish(func(ctx context.Context) error {
			return w.store.ConditionalFinalizeSuccess(ctx, job.ID, w.cfg.WorkerID, *job.ClaimToken)
		}, successFinalizationExpected)
		return
	}
	if errors.Is(err, ErrAlreadyFinalized) {
		cancelJob(context.Canceled)
		renewer.Stop()
		return
	}

	// Handle failure or cancellation
	errClass, errCode := ClassifyError(err)
	if errClass == ErrorClassCancelled {
		log.Info("job execution was cancelled", "error", err)
		finish(func(ctx context.Context) error {
			return w.store.ConditionalFinalizeCancel(ctx, job.ID, w.cfg.WorkerID, *job.ClaimToken)
		}, cancelledFinalizationExpected)
		return
	}

	if errors.Is(err, ErrOwnershipLost) || errClass == ErrorClassOwnershipLost {
		cancelJob(context.Canceled)
		renewer.Stop()
		log.Warn("job execution aborted because ownership was lost", "error", err)
		return
	}

	isTerminal := (errClass == ErrorClassPermanent) || (job.AttemptCount >= job.MaxAttempts)
	var termReason *TerminalReason
	var nextRun time.Time

	if isTerminal {
		if errClass == ErrorClassPermanent {
			tr := TerminalReasonPermanent
			termReason = &tr
		} else {
			tr := TerminalReasonRetryableExhausted
			termReason = &tr
		}
		log.Warn("job execution failed permanently", "terminal_reason", *termReason, "error", err)
	} else {
		backoff := CalculateBackoff(job.AttemptCount, w.cfg.BaseBackoff, w.cfg.MaxBackoff)
		nextRun = time.Now().UTC().Add(backoff)
		log.Warn("job execution failed, scheduled retry", "next_run_at", nextRun, "error", err)
	}

	message := err.Error()
	finish(func(ctx context.Context) error {
		return w.store.ConditionalFinalizeFailure(ctx, job.ID, w.cfg.WorkerID, *job.ClaimToken,
			errClass, errCode, message, termReason, isTerminal, nextRun)
	}, failureFinalizationExpected(errClass, errCode, termReason, isTerminal))
}

func (w *Worker) resolveFinalization(ctx context.Context, claim *AnalysisJob, finalize func(context.Context) error, expected func(*AnalysisJob) bool) error {
	var lastErr error
	for {
		if err := context.Cause(ctx); err != nil {
			if lastErr != nil {
				return fmt.Errorf("finalization context ended after %v: %w", lastErr, err)
			}
			return err
		}

		opCtx, cancel := context.WithTimeout(ctx, w.finalizationOperationTimeout())
		err := finalize(opCtx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		readCtx, cancelRead := context.WithTimeout(ctx, w.finalizationOperationTimeout())
		current, readErr := w.store.GetJobByID(readCtx, claim.ID)
		cancelRead()
		if errors.Is(readErr, sql.ErrNoRows) {
			return ErrOwnershipLost
		}
		if readErr == nil {
			if sameExecutionClaim(claim, current, w.cfg.WorkerID) {
				if expected(current) {
					return nil
				}
				if current.Status != StatusRunning || !current.ExecutionStarted {
					return fmt.Errorf("job %d left the active claim without reaching the expected terminal state: status=%s", claim.ID, current.Status)
				}
			} else {
				return ErrOwnershipLost
			}
		} else {
			lastErr = fmt.Errorf("finalization failed (%v) and durable state could not be read: %w", err, readErr)
		}

		logger.L(ctx).Error("job terminal finalization failed; retrying claim-fenced transition", "job_id", claim.ID, "worker_id", w.cfg.WorkerID, "error", lastErr)
		timer := time.NewTimer(w.finalizationRetryDelay())
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
	}
}

func sameExecutionClaim(claim, current *AnalysisJob, workerID string) bool {
	if claim == nil || current == nil || claim.ClaimToken == nil || current.ClaimToken == nil || current.WorkerID == nil {
		return false
	}
	return claim.ExecutionStarted && current.ExecutionStarted && *current.WorkerID == workerID &&
		*current.ClaimToken == *claim.ClaimToken &&
		current.ExecutionGeneration == claim.ExecutionGeneration
}

func successFinalizationExpected(job *AnalysisJob) bool {
	return job != nil && (job.Status == StatusSucceeded ||
		(job.Status == StatusCancelled && job.CancelRequested && job.TerminalReason != nil && *job.TerminalReason == TerminalReasonCancelled))
}

func cancelledFinalizationExpected(job *AnalysisJob) bool {
	return job != nil && job.Status == StatusCancelled && job.CancelRequested && job.TerminalReason != nil && *job.TerminalReason == TerminalReasonCancelled
}

func failureFinalizationExpected(class ErrorClass, code string, reason *TerminalReason, terminal bool) func(*AnalysisJob) bool {
	return func(job *AnalysisJob) bool {
		if job == nil {
			return false
		}
		if cancelledFinalizationExpected(job) {
			return true
		}
		if (terminal && job.Status != StatusFailed) || (!terminal && job.Status != StatusRetryWait) {
			return false
		}
		if job.LastErrorClass == nil || *job.LastErrorClass != string(class) || job.LastErrorCode == nil || *job.LastErrorCode != code {
			return false
		}
		if terminal {
			return reason != nil && job.TerminalReason != nil && *job.TerminalReason == *reason
		}
		return true
	}
}

func (w *Worker) finalizationOperationTimeout() time.Duration {
	timeout := w.cfg.LeaseDuration / 4
	if timeout < 500*time.Millisecond {
		timeout = 500 * time.Millisecond
	}
	if timeout > time.Second {
		timeout = time.Second
	}
	return timeout
}

func (w *Worker) finalizationRetryDelay() time.Duration {
	delay := w.cfg.PollInterval
	if delay < 25*time.Millisecond {
		delay = 25 * time.Millisecond
	}
	if delay > time.Second {
		delay = time.Second
	}
	return delay
}

func (w *Worker) startFinalizationLeaseRenewer(ctx context.Context, job *AnalysisJob) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	interval := w.cfg.LeaseDuration / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-ticker.C:
				opCtx, cancel := context.WithTimeout(ctx, w.finalizationOperationTimeout())
				err := w.store.RenewLease(opCtx, job.ID, w.cfg.WorkerID, *job.ClaimToken, time.Now().UTC().Add(w.cfg.LeaseDuration))
				cancel()
				if err != nil && !errors.Is(err, ErrOwnershipLost) {
					logger.L(ctx).Warn("lease renewal failed while resolving terminal finalization", "job_id", job.ID, "worker_id", w.cfg.WorkerID, "error", err)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(stop) })
		<-done
	}
}

func (w *Worker) reapLoop(ctx context.Context) {
	defer w.loopWG.Done()
	ticker := time.NewTicker(w.cfg.ReapInterval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			reaped, err := w.store.ReapExpiredJobs(ctx, 50)
			if err != nil {
				log := logger.L(ctx)
				log.Error("error reaping expired jobs", "error", err)
			} else if reaped > 0 {
				log := logger.L(ctx)
				log.Info("reaped expired jobs", "count", reaped)
			}
		}
	}
}
