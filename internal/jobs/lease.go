package jobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"repolens/internal/platform/logger"
)

// LeaseRenewer runs a background ticker to renew a job's lease.
type LeaseRenewer struct {
	store interface {
		RenewLease(context.Context, int64, string, string, time.Time) error
	}
	jobID         int64
	workerID      string
	claimToken    string
	leaseDuration time.Duration
	cancelFunc    context.CancelCauseFunc
	stopCh        chan struct{}
	doneCh        chan struct{}
	stopOnce      sync.Once
}

// StartLeaseRenewer creates and starts a new lease renewer.
func StartLeaseRenewer(ctx context.Context, store interface {
	RenewLease(context.Context, int64, string, string, time.Time) error
}, jobID int64, workerID, claimToken string, leaseDuration time.Duration, cancelFunc context.CancelCauseFunc) *LeaseRenewer {
	lr := &LeaseRenewer{
		store:         store,
		jobID:         jobID,
		workerID:      workerID,
		claimToken:    claimToken,
		leaseDuration: leaseDuration,
		cancelFunc:    cancelFunc,
		stopCh:        make(chan struct{}),
		doneCh:        make(chan struct{}),
	}
	go lr.run(ctx)
	return lr
}

func (lr *LeaseRenewer) run(ctx context.Context) {
	defer close(lr.doneCh)
	interval := lr.leaseDuration / 3
	if interval < 100*time.Millisecond {
		interval = 100 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-lr.stopCh:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			newLeaseUntil := time.Now().UTC().Add(lr.leaseDuration)
			err := lr.store.RenewLease(ctx, lr.jobID, lr.workerID, lr.claimToken, newLeaseUntil)
			if err != nil {
				log := logger.L(ctx)
				log.Warn("lease renewal failed", "job_id", lr.jobID, "worker_id", lr.workerID, "error", err)
				if lr.cancelFunc != nil {
					if errors.Is(err, ErrOwnershipLost) {
						lr.cancelFunc(ErrOwnershipLost)
					} else {
						lr.cancelFunc(fmt.Errorf("%w: %w", ErrLeaseRenewFailed, err))
					}
				}
				return
			}
		}
	}
}

// Stop stops the lease renewer and waits for it to exit.
func (lr *LeaseRenewer) Stop() {
	lr.stopOnce.Do(func() { close(lr.stopCh) })
	<-lr.doneCh
}
