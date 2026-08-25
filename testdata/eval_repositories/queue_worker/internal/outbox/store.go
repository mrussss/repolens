package outbox

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Store interface {
	CreateInTx(ctx context.Context, tx *gorm.DB, event *OutboxEvent) error
	FetchPending(ctx context.Context, limit int) ([]OutboxEvent, error)
	MarkPublished(ctx context.Context, id string) error
	MarkFailed(ctx context.Context, id string, errStr string) error
}

type GormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) CreateInTx(ctx context.Context, tx *gorm.DB, event *OutboxEvent) error {
	if event.ID == "" {
		event.ID = uuid.New().String()
	}
	if event.AvailableAt.IsZero() {
		event.AvailableAt = time.Now()
	}
	if event.Status == "" {
		event.Status = StatusPending
	}
	return tx.WithContext(ctx).Create(event).Error
}

func (s *GormStore) FetchPending(ctx context.Context, limit int) ([]OutboxEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	var events []OutboxEvent
	now := time.Now()

	err := s.db.WithContext(ctx).
		Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
		Where("status = ? AND available_at <= ?", StatusPending, now).
		Order("available_at ASC").
		Limit(limit).
		Find(&events).Error

	if err != nil {
		// Fallback for database engines (like SQLite in dev) that don't support SKIP LOCKED
		return s.fetchPendingFallback(ctx, limit, now)
	}
	return events, nil
}

func (s *GormStore) fetchPendingFallback(ctx context.Context, limit int, now time.Time) ([]OutboxEvent, error) {
	var events []OutboxEvent
	err := s.db.WithContext(ctx).
		Where("status = ? AND available_at <= ?", StatusPending, now).
		Order("available_at ASC").
		Limit(limit).
		Find(&events).Error
	return events, err
}

func (s *GormStore) MarkPublished(ctx context.Context, id string) error {
	now := time.Now()
	return s.db.WithContext(ctx).Model(&OutboxEvent{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"status":       StatusPublished,
			"published_at": &now,
		}).Error
}

func (s *GormStore) MarkFailed(ctx context.Context, id string, errStr string) error {
	return s.db.WithContext(ctx).Model(&OutboxEvent{}).
		Where("id = ?", id).
		Updates(map[string]interface{}{
			"status":      StatusFailed,
			"retry_count": gorm.Expr("retry_count + 1"),
		}).Error
}
