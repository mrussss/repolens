package outbox

import (
	"context"
	"time"

	"repolens/internal/mq"
	"repolens/internal/platform/logger"
)

type Relay struct {
	store     Store
	broker    mq.Broker
	interval  time.Duration
	batchSize int
}

func NewRelay(store Store, broker mq.Broker, interval time.Duration, batchSize int) *Relay {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	if batchSize <= 0 {
		batchSize = 50
	}
	return &Relay{
		store:     store,
		broker:    broker,
		interval:  interval,
		batchSize: batchSize,
	}
}

func (r *Relay) Start(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.ProcessPending(ctx)
		}
	}
}

func (r *Relay) ProcessPending(ctx context.Context) {
	events, err := r.store.FetchPending(ctx, r.batchSize)
	if err != nil {
		logger.L(ctx).Error("failed to fetch pending outbox events", "error", err)
		return
	}

	for _, evt := range events {
		var queue string
		switch evt.AggregateType {
		case AggregateDiagnosisRun:
			queue = mq.QueueDiagnosisTask
		case AggregateRepositoryIndex:
			queue = mq.QueueIndexTask
		default:
			queue = mq.QueueDiagnosisTask
		}

		msg := mq.Message{
			ID:        evt.ID,
			EventType: evt.EventType,
			Payload:   evt.Payload,
		}

		if err := r.broker.Publish(ctx, queue, msg); err != nil {
			logger.L(ctx).Error("failed to publish outbox event to queue", "event_id", evt.ID, "error", err)
			_ = r.store.MarkFailed(ctx, evt.ID, err.Error())
			continue
		}

		if err := r.store.MarkPublished(ctx, evt.ID); err != nil {
			logger.L(ctx).Error("failed to mark outbox event published", "event_id", evt.ID, "error", err)
		}
	}
}
