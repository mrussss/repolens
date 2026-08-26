package jobs

import (
	"context"
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
	WorkerID      string
	Concurrency   int
	BatchSize     int
	PollInterval  time.Duration
	LeaseDuration time.Duration
	ReapInterval  time.Duration
	BaseBackoff   time.Duration
	MaxBackoff    time.Duration
}

// DefaultWorkerConfig returns production defaults for the worker.
func DefaultWorkerConfig() WorkerConfig {
	return WorkerConfig{
		WorkerID:      "worker-" + uuid.New().String()[:8],
		Concurrency:   4,
		BatchSize:     4,
		PollInterval:  time.Second,
		LeaseDuration: 30 * time.Second,
		ReapInterval:  10 * time.Second,
		BaseBackoff:   2 * time.Second,
		MaxBackoff:    60 * time.Second,
	}
}

// Worker executes async jobs claimed from the store.
type Worker struct {
	store    *Store
	cfg      WorkerConfig
	handlers map[JobType]Handler
	mu       sync.RWMutex
	wg       sync.WaitGroup
	stopCh   chan struct{}
}

// NewWorker constructs a new Worker.
func NewWorker(store *Store, cfg WorkerConfig) *Worker {
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

	return &Worker{
		store:    store,
		cfg:      cfg,
		handlers: make(map[JobType]Handler),
		stopCh:   make(chan struct{}),
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

	w.wg.Add(2)
	go w.claimLoop(ctx)
	go w.reapLoop(ctx)
}

// Stop gracefully shuts down the worker, waiting for active jobs to finish.
func (w *Worker) Stop() {
	close(w.stopCh)
	w.wg.Wait()
}

func (w *Worker) claimLoop(ctx context.Context) {
	defer w.wg.Done()
	sem := make(chan struct{}, w.cfg.Concurrency)

	for {
		select {
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		// Claim batch
		jobs, err := w.store.ClaimJobs(ctx, w.cfg.WorkerID, w.cfg.BatchSize, w.cfg.LeaseDuration)
		if err != nil {
			log := logger.L(ctx)
			log.Error("error claiming jobs", "worker_id", w.cfg.WorkerID, "error", err)
			time.Sleep(w.cfg.PollInterval)
			continue
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

		for _, job := range jobs {
			sem <- struct{}{}
			w.wg.Add(1)
			go func(j *AnalysisJob) {
				defer func() {
					<-sem
					w.wg.Done()
				}()
				w.executeJob(ctx, j)
			}(job)
		}
	}
}

func (w *Worker) executeJob(parentCtx context.Context, job *AnalysisJob) {
	log := logger.L(parentCtx).With(
		"job_id", job.ID,
		"job_type", string(job.JobType),
		"resource_id", job.ResourceID,
		"attempt", job.AttemptCount,
		"worker_id", w.cfg.WorkerID,
	)

	w.mu.RLock()
	handler, exists := w.handlers[job.JobType]
	w.mu.RUnlock()

	if !exists {
		log.Error("no handler registered for job type", "job_type", job.JobType)
		termReason := TerminalReasonPermanent
		_ = w.store.ConditionalFinalizeFailure(
			parentCtx, job.ID, w.cfg.WorkerID, *job.ClaimToken,
			ErrorClassPermanent, "NO_HANDLER_REGISTERED",
			fmt.Sprintf("no handler registered for job type %s", job.JobType),
			&termReason, true, time.Time{},
		)
		return
	}

	// Create cancellable execution context
	jobCtx, cancelJob := context.WithCancel(parentCtx)
	defer cancelJob()

	// Start background lease renewer
	renewer := StartLeaseRenewer(jobCtx, w.store, job.ID, w.cfg.WorkerID, *job.ClaimToken, w.cfg.LeaseDuration, cancelJob)
	defer renewer.Stop()

	// Check if cancellation was requested before execution
	if job.CancelRequested {
		log.Info("job was cancel_requested prior to execution")
		finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelFinalize()
		_ = w.store.ConditionalFinalizeCancel(finalizeCtx, job.ID, w.cfg.WorkerID, *job.ClaimToken)
		return
	}

	start := time.Now()
	err := handler.Execute(jobCtx, job)
	latency := time.Since(start)

	finalizeCtx, cancelFinalize := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelFinalize()

	if err == nil {
		log.Info("job execution succeeded", "latency_ms", latency.Milliseconds())
		finalErr := w.store.ConditionalFinalizeSuccess(finalizeCtx, job.ID, w.cfg.WorkerID, *job.ClaimToken)
		if finalErr != nil && errors.Is(finalErr, ErrOwnershipLost) {
			log.Warn("finalize success skipped due to ownership loss", "error", finalErr)
		}
		return
	}

	// Handle failure or cancellation
	errClass, errCode := ClassifyError(err)
	if errors.Is(err, context.Canceled) || errClass == ErrorClassCancelled {
		log.Info("job execution was cancelled", "error", err)
		_ = w.store.ConditionalFinalizeCancel(finalizeCtx, job.ID, w.cfg.WorkerID, *job.ClaimToken)
		return
	}

	if errors.Is(err, ErrOwnershipLost) || errClass == ErrorClassOwnershipLost {
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

	_ = w.store.ConditionalFinalizeFailure(
		finalizeCtx, job.ID, w.cfg.WorkerID, *job.ClaimToken,
		errClass, errCode, err.Error(),
		termReason, isTerminal, nextRun,
	)
}

func (w *Worker) reapLoop(ctx context.Context) {
	defer w.wg.Done()
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
