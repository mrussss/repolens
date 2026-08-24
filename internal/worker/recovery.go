package worker

import (
	"context"
	"time"

	"repolens/internal/diagnosis"
	"repolens/internal/platform/logger"
)

type HeartbeatEmitter struct {
	store     diagnosis.Store
	attemptID string
	interval  time.Duration
	stopCh    chan struct{}
}

func NewHeartbeatEmitter(store diagnosis.Store, attemptID string, interval time.Duration) *HeartbeatEmitter {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &HeartbeatEmitter{
		store:     store,
		attemptID: attemptID,
		interval:  interval,
		stopCh:    make(chan struct{}),
	}
}

func (h *HeartbeatEmitter) Start(ctx context.Context) {
	ticker := time.NewTicker(h.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-h.stopCh:
			return
		case <-ticker.C:
			if err := h.store.UpdateAttemptHeartbeat(ctx, h.attemptID, time.Now()); err != nil {
				logger.L(ctx).Warn("failed to update attempt heartbeat", "attempt_id", h.attemptID, "error", err)
			}
		}
	}
}

func (h *HeartbeatEmitter) Stop() {
	select {
	case <-h.stopCh:
	default:
		close(h.stopCh)
	}
}

type RecoverySweeper struct {
	store         diagnosis.Store
	staleDuration time.Duration
	checkInterval time.Duration
	retryBackoff  time.Duration
}

func NewRecoverySweeper(store diagnosis.Store, staleDuration, checkInterval, retryBackoff time.Duration) *RecoverySweeper {
	if staleDuration <= 0 {
		staleDuration = 30 * time.Second
	}
	if checkInterval <= 0 {
		checkInterval = 10 * time.Second
	}
	if retryBackoff <= 0 {
		retryBackoff = 2 * time.Second
	}
	return &RecoverySweeper{
		store:         store,
		staleDuration: staleDuration,
		checkInterval: checkInterval,
		retryBackoff:  retryBackoff,
	}
}

func (s *RecoverySweeper) Start(ctx context.Context) {
	ticker := time.NewTicker(s.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.SweepOnce(ctx)
		}
	}
}

func (s *RecoverySweeper) SweepOnce(ctx context.Context) int {
	staleAttempts, err := s.store.FetchStaleAttempts(ctx, s.staleDuration, 20)
	if err != nil {
		logger.L(ctx).Error("failed to fetch stale attempts for recovery", "error", err)
		return 0
	}

	recovered := 0
	for _, att := range staleAttempts {
		logger.L(ctx).Warn("recovering stale attempt from crashed worker",
			"attempt_id", att.ID,
			"run_id", att.DiagnosisRunID,
			"worker_id", att.WorkerID,
			"last_heartbeat", att.HeartbeatAt,
		)
		if err := s.store.RecoverStaleAttempt(ctx, att.ID, att.DiagnosisRunID, s.retryBackoff); err != nil {
			logger.L(ctx).Error("failed to recover stale attempt", "attempt_id", att.ID, "error", err)
		} else {
			recovered++
		}
	}
	return recovered
}
