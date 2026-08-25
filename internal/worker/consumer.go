package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/mq"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/metrics"
	"repolens/internal/sse"
)

type DiagnosisPayload struct {
	DiagnosisRunID string `json:"diagnosis_run_id"`
	RepositoryID   string `json:"repository_id"`
	SnapshotID     string `json:"snapshot_id"`
	UserID         string `json:"user_id"`
	RetryCount     int    `json:"retry_count,omitempty"`
}

type ConsumerConfig struct {
	WorkerID        string
	Prefetch        int
	MaxAttempts     int
	AttemptDeadline time.Duration
	RetryBackoff    time.Duration
}

type DiagnosisConsumer struct {
	cfg            ConsumerConfig
	broker         mq.Broker
	diagnosisStore diagnosis.Store
	reportStore    evidence.ReportStore
	citationStore  evidence.CitationStore
	citationVal    *evidence.CitationValidator
	executor       DiagnosisExecutor
	sseHub         *sse.Hub
	wg             sync.WaitGroup
}

func NewDiagnosisConsumer(
	cfg ConsumerConfig,
	broker mq.Broker,
	diagnosisStore diagnosis.Store,
	reportStore evidence.ReportStore,
	citationStore evidence.CitationStore,
	citationVal *evidence.CitationValidator,
	executor DiagnosisExecutor,
	sseHub *sse.Hub,
) *DiagnosisConsumer {
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker-" + uuid.New().String()[:8]
	}
	if cfg.Prefetch <= 0 {
		cfg.Prefetch = 5
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.AttemptDeadline <= 0 {
		cfg.AttemptDeadline = 5 * time.Minute
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 5 * time.Second
	}

	return &DiagnosisConsumer{
		cfg:            cfg,
		broker:         broker,
		diagnosisStore: diagnosisStore,
		reportStore:    reportStore,
		citationStore:  citationStore,
		citationVal:    citationVal,
		executor:       executor,
		sseHub:         sseHub,
	}
}

func (c *DiagnosisConsumer) Start(ctx context.Context) error {
	msgCh, err := c.broker.Consume(ctx, mq.QueueDiagnosisTask, c.cfg.Prefetch)
	if err != nil {
		return fmt.Errorf("failed to start consuming diagnosis queue: %w", err)
	}

	logger.L(ctx).Info("diagnosis consumer started", "worker_id", c.cfg.WorkerID)

	for {
		select {
		case <-ctx.Done():
			c.wg.Wait()
			return nil
		case msg, ok := <-msgCh:
			if !ok {
				c.wg.Wait()
				if ctx.Err() == nil {
					return errors.New("diagnosis message channel closed unexpectedly by broker")
				}
				return nil
			}
			c.wg.Add(1)
			go func(m mq.Message) {
				defer c.wg.Done()
				c.handleMessage(ctx, m)
			}(msg)
		}
	}
}

func (c *DiagnosisConsumer) handleMessage(parentCtx context.Context, msg mq.Message) {
	metrics.WorkerInflight.Inc()
	defer metrics.WorkerInflight.Dec()

	if msg.Redelivered {
		metrics.MQRedeliveryTotal.Inc()
	}
	startProcess := time.Now()

	var payload DiagnosisPayload
	if err := json.Unmarshal([]byte(msg.Payload), &payload); err != nil {
		logger.L(parentCtx).Error("malformed diagnosis message, routing to DLQ", "msg_id", msg.ID, "error", err)
		_ = c.broker.PublishDLQ(parentCtx, mq.QueueDiagnosisTask, msg, "malformed_json: "+err.Error())
		if msg.AckFunc != nil {
			_ = msg.AckFunc()
		}
		return
	}

	if payload.DiagnosisRunID == "" {
		logger.L(parentCtx).Error("missing diagnosis_run_id, routing to DLQ", "msg_id", msg.ID)
		_ = c.broker.PublishDLQ(parentCtx, mq.QueueDiagnosisTask, msg, "missing_diagnosis_run_id")
		if msg.AckFunc != nil {
			_ = msg.AckFunc()
		}
		return
	}

	ctx := context.WithValue(parentCtx, logger.DiagnosisIDKey, payload.DiagnosisRunID)

	// Attempt to claim run
	expectedStatuses := []diagnosis.RunStatus{diagnosis.StatusQueued, diagnosis.StatusRetryWait}
	run, attempt, err := c.diagnosisStore.ClaimRun(ctx, payload.DiagnosisRunID, expectedStatuses, c.cfg.WorkerID, c.cfg.AttemptDeadline)
	if err != nil {
		if errors.Is(err, diagnosis.ErrClaimConflict) {
			// Idempotency: Check if already completed or active
			existingRun, err := c.diagnosisStore.GetByID(ctx, payload.DiagnosisRunID)
			if err == nil && (existingRun.Status == diagnosis.StatusSucceeded || existingRun.Status == diagnosis.StatusFailed || existingRun.Status == diagnosis.StatusCancelled) {
				logger.L(ctx).Info("run already in terminal state, acking duplicate message", "run_id", payload.DiagnosisRunID, "status", existingRun.Status)
				if msg.AckFunc != nil {
					_ = msg.AckFunc()
				}
				return
			}
			logger.L(ctx).Warn("claim conflict for run (may be claimed by another worker or in progress)", "run_id", payload.DiagnosisRunID)
			if msg.AckFunc != nil {
				_ = msg.AckFunc()
			}
			return
		}
		logger.L(ctx).Error("failed to claim run", "run_id", payload.DiagnosisRunID, "error", err)
		if msg.AckFunc != nil {
			_ = msg.AckFunc()
		}
		return
	}

	ctx = context.WithValue(ctx, logger.AttemptIDKey, attempt.ID)
	logger.L(ctx).Info("claimed diagnosis run", "run_id", run.ID, "attempt_id", attempt.ID, "attempt_no", attempt.AttemptNo)

	// Publish SSE attempt started
	if c.sseHub != nil {
		c.sseHub.Publish(run.ID, sse.Event{
			Type:      "attempt.started",
			Timestamp: time.Now(),
			Data: map[string]interface{}{
				"run_id":     run.ID,
				"attempt_id": attempt.ID,
				"attempt_no": attempt.AttemptNo,
				"worker_id":  c.cfg.WorkerID,
			},
		})
	}

	// Start Heartbeat
	hb := NewHeartbeatEmitter(c.diagnosisStore, attempt.ID, 3*time.Second)
	go hb.Start(ctx)
	defer hb.Stop()

	// Execution context with deadline
	execCtx, cancel := context.WithTimeout(ctx, c.cfg.AttemptDeadline)
	defer cancel()

	result, execErr := c.executor.Execute(execCtx, run, attempt)

	// Check cancellation
	refreshedRun, _ := c.diagnosisStore.GetByID(ctx, run.ID)
	if (refreshedRun != nil && refreshedRun.CancelRequested) || errors.Is(execErr, context.Canceled) {
		logger.L(ctx).Info("diagnosis run was cancelled", "run_id", run.ID, "attempt_id", attempt.ID)
		_ = c.diagnosisStore.ConfirmCancellation(ctx, run.ID, attempt.ID)
		if c.sseHub != nil {
			c.sseHub.Publish(run.ID, sse.Event{
				Type:      "diagnosis.cancelled",
				Timestamp: time.Now(),
				Data:      map[string]interface{}{"run_id": run.ID},
			})
		}
		if msg.AckFunc != nil {
			_ = msg.AckFunc()
		}
		return
	}

	if execErr != nil {
		isRetryable := result != nil && result.Retryable
		if !isRetryable && errors.Is(execErr, context.DeadlineExceeded) {
			isRetryable = true
		}

		// Check if max attempts reached
		if isRetryable && attempt.AttemptNo < c.cfg.MaxAttempts {
			metrics.ApplicationRetryTotal.Inc()
			backoff := c.cfg.RetryBackoff * time.Duration(attempt.AttemptNo)
			logger.L(ctx).Warn("attempt failed with retryable error, scheduling retry",
				"run_id", run.ID,
				"attempt_id", attempt.ID,
				"attempt_no", attempt.AttemptNo,
				"error", execErr,
				"backoff", backoff,
			)
			_ = c.diagnosisStore.FinishAttemptAndRun(
				ctx,
				run.ID,
				attempt.ID,
				diagnosis.StatusRetryWait,
				diagnosis.AttemptStatusFailedRetryable,
				0, 0, 0,
				"RETRYABLE_ERROR",
				execErr.Error(),
				true,
				backoff,
			)
		} else {
			metrics.DiagnosisFailedTotal.WithLabelValues("TERMINAL_ERROR").Inc()
			logger.L(ctx).Error("attempt failed terminally or retries exhausted",
				"run_id", run.ID,
				"attempt_id", attempt.ID,
				"attempt_no", attempt.AttemptNo,
				"error", execErr,
			)
			_ = c.diagnosisStore.FinishAttemptAndRun(
				ctx,
				run.ID,
				attempt.ID,
				diagnosis.StatusFailed,
				diagnosis.AttemptStatusFailedTerminal,
				0, 0, 0,
				"TERMINAL_ERROR",
				execErr.Error(),
				false,
				0,
			)
			if c.sseHub != nil {
				c.sseHub.Publish(run.ID, sse.Event{
					Type:      "diagnosis.failed",
					Timestamp: time.Now(),
					Data:      map[string]interface{}{"run_id": run.ID, "error": execErr.Error()},
				})
			}
		}

		if msg.AckFunc != nil {
			_ = msg.AckFunc()
		}
		return
	}

	// Success: Process Report and Citations
	if result != nil && result.Report != nil {
		findingsBytes, _ := json.Marshal(result.Report.Findings)
		checksBytes, _ := json.Marshal(result.Report.RecommendedChecks)

		rep := &evidence.Report{
			ID:                    uuid.New().String(),
			DiagnosisRunID:        run.ID,
			AttemptID:             attempt.ID,
			RootCause:             result.Report.RootCause,
			FindingsJSON:          string(findingsBytes),
			RecommendedChecksJSON: string(checksBytes),
			Confidence:            result.Report.Confidence,
			CreatedAt:             time.Now(),
		}
		if err := c.reportStore.Create(ctx, rep); err != nil {
			logger.L(ctx).Error("failed to save report", "run_id", run.ID, "error", err)
		}

		// Validate and save citations
		var allCitations []evidence.Citation
		for _, f := range result.Report.Findings {
			for _, cit := range f.Citations {
				cit.ReportID = rep.ID
				cit.SnapshotID = run.SnapshotID
				cit.CreatedAt = time.Now()
				if c.citationVal != nil {
					c.citationVal.Validate(ctx, run.RepositoryID, run.SnapshotID, &cit)
				}
				allCitations = append(allCitations, cit)
			}
		}
		if len(allCitations) > 0 {
			if err := c.citationStore.CreateBatch(ctx, allCitations); err != nil {
				logger.L(ctx).Error("failed to save citations", "run_id", run.ID, "error", err)
			}
		}
	}

	promptTokens := 0
	completionTokens := 0
	toolCalls := 0
	if result != nil {
		promptTokens = result.PromptTokens
		completionTokens = result.CompletionTokens
		toolCalls = result.ToolCalls
	}

	// Finalize status to SUCCEEDED
	err = c.diagnosisStore.FinishAttemptAndRun(
		ctx,
		run.ID,
		attempt.ID,
		diagnosis.StatusSucceeded,
		diagnosis.AttemptStatusSucceeded,
		promptTokens,
		completionTokens,
		toolCalls,
		"",
		"",
		false,
		0,
	)
	if err != nil {
		logger.L(ctx).Error("failed to finalize run status to SUCCEEDED", "run_id", run.ID, "error", err)
	} else {
		metrics.DiagnosisLatencySeconds.Observe(time.Since(startProcess).Seconds())
	}

	if c.sseHub != nil {
		c.sseHub.Publish(run.ID, sse.Event{
			Type:      "report.completed",
			Timestamp: time.Now(),
			Data: map[string]interface{}{
				"run_id":     run.ID,
				"attempt_id": attempt.ID,
				"status":     diagnosis.StatusSucceeded,
			},
		})
	}

	// ACK message
	if msg.AckFunc != nil {
		if err := msg.AckFunc(); err != nil {
			logger.L(ctx).Error("failed to ack message after successful processing", "error", err)
		}
	}
	logger.L(ctx).Info("diagnosis run completed successfully", "run_id", run.ID, "attempt_id", attempt.ID)
}
