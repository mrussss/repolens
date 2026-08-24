package diagnosis

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"repolens/internal/outbox"
)

var (
	ErrIdempotencyConflict = errors.New("idempotency conflict: request payload differs from existing record")
	ErrRunNotFound         = errors.New("diagnosis run not found")
	ErrAttemptNotFound     = errors.New("diagnosis attempt not found")
	ErrClaimConflict       = errors.New("run claim conflict: status is not in expected state or already claimed")
	ErrOptimisticLock      = errors.New("optimistic lock conflict")
)

type Store interface {
	CreateWithOutbox(ctx context.Context, run *DiagnosisRun, outboxEvent *outbox.OutboxEvent) error
	GetByID(ctx context.Context, id string) (*DiagnosisRun, error)
	GetByIDAndUser(ctx context.Context, id, userID string) (*DiagnosisRun, error)
	GetByIdempotencyKey(ctx context.Context, userID, key string) (*DiagnosisRun, error)
	ListByUser(ctx context.Context, userID string, page, pageSize int) ([]DiagnosisRun, int64, error)
	ClaimRun(ctx context.Context, runID string, expectedStatuses []RunStatus, workerID string, attemptDeadline time.Duration) (*DiagnosisRun, *DiagnosisAttempt, error)
	GetAttempt(ctx context.Context, attemptID string) (*DiagnosisAttempt, error)
	ListAttemptsByRun(ctx context.Context, runID string) ([]DiagnosisAttempt, error)
	UpdateAttemptHeartbeat(ctx context.Context, attemptID string, heartbeatAt time.Time) error
	FinishAttemptAndRun(ctx context.Context, runID, attemptID string, newRunStatus RunStatus, newAttemptStatus AttemptStatus, promptTokens, completionTokens, toolCalls int, errCode, errMsg string, retryable bool, retryDelay time.Duration) error
	RequestCancellation(ctx context.Context, runID, userID string) error
	ConfirmCancellation(ctx context.Context, runID, attemptID string) error
	FetchStaleAttempts(ctx context.Context, staleDuration time.Duration, limit int) ([]DiagnosisAttempt, error)
	RecoverStaleAttempt(ctx context.Context, attemptID, runID string, backoff time.Duration) error
}

type GormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) CreateWithOutbox(ctx context.Context, run *DiagnosisRun, outboxEvent *outbox.OutboxEvent) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if run.ID == "" {
			run.ID = uuid.New().String()
		}
		run.Status = StatusQueued
		run.Version = 1

		if err := tx.Create(run).Error; err != nil {
			return err
		}

		if outboxEvent != nil {
			if outboxEvent.ID == "" {
				outboxEvent.ID = uuid.New().String()
			}
			outboxEvent.AggregateType = outbox.AggregateDiagnosisRun
			outboxEvent.AggregateID = run.ID
			outboxEvent.EventType = outbox.EventDiagnosisRequested
			if outboxEvent.Payload == "" {
				payload, _ := json.Marshal(map[string]interface{}{
					"diagnosis_run_id": run.ID,
					"repository_id":    run.RepositoryID,
					"snapshot_id":      run.SnapshotID,
					"user_id":          run.UserID,
				})
				outboxEvent.Payload = string(payload)
			}
			if outboxEvent.AvailableAt.IsZero() {
				outboxEvent.AvailableAt = time.Now()
			}
			if outboxEvent.Status == "" {
				outboxEvent.Status = outbox.StatusPending
			}
			if err := tx.Create(outboxEvent).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *GormStore) GetByID(ctx context.Context, id string) (*DiagnosisRun, error) {
	var run DiagnosisRun
	if err := s.db.WithContext(ctx).First(&run, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	return &run, nil
}

func (s *GormStore) GetByIDAndUser(ctx context.Context, id, userID string) (*DiagnosisRun, error) {
	var run DiagnosisRun
	if err := s.db.WithContext(ctx).First(&run, "id = ? AND user_id = ?", id, userID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrRunNotFound
		}
		return nil, err
	}
	return &run, nil
}

func (s *GormStore) GetByIdempotencyKey(ctx context.Context, userID, key string) (*DiagnosisRun, error) {
	var run DiagnosisRun
	if err := s.db.WithContext(ctx).First(&run, "user_id = ? AND idempotency_key = ?", userID, key).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &run, nil
}

func (s *GormStore) ListByUser(ctx context.Context, userID string, page, pageSize int) ([]DiagnosisRun, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	var runs []DiagnosisRun
	var total int64

	tx := s.db.WithContext(ctx).Model(&DiagnosisRun{}).Where("user_id = ?", userID)
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if err := tx.Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&runs).Error; err != nil {
		return nil, 0, err
	}
	return runs, total, nil
}

func (s *GormStore) ClaimRun(ctx context.Context, runID string, expectedStatuses []RunStatus, workerID string, attemptDeadline time.Duration) (*DiagnosisRun, *DiagnosisAttempt, error) {
	var claimedRun DiagnosisRun
	var attempt DiagnosisAttempt

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run DiagnosisRun
		if err := tx.First(&run, "id = ?", runID).Error; err != nil {
			return err
		}

		// Check if in expected status
		valid := false
		for _, st := range expectedStatuses {
			if run.Status == st {
				valid = true
				break
			}
		}
		if !valid {
			return ErrClaimConflict
		}

		// Check cancellation
		if run.CancelRequested {
			return fmt.Errorf("run %s has been requested for cancellation", runID)
		}

		// Count existing attempts for attempt_no
		var count int64
		if err := tx.Model(&DiagnosisAttempt{}).Where("diagnosis_run_id = ?", runID).Count(&count).Error; err != nil {
			return err
		}

		now := time.Now()
		deadline := now.Add(attemptDeadline)

		// Conditional update run to RUNNING
		res := tx.Model(&DiagnosisRun{}).
			Where("id = ? AND version = ? AND status = ?", runID, run.Version, run.Status).
			Updates(map[string]interface{}{
				"status":  StatusRunning,
				"version": run.Version + 1,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrClaimConflict
		}

		// Create DiagnosisAttempt
		attempt = DiagnosisAttempt{
			ID:             uuid.New().String(),
			DiagnosisRunID: runID,
			AttemptNo:      int(count) + 1,
			WorkerID:       workerID,
			Status:         AttemptStatusRunning,
			StartedAt:      now,
			HeartbeatAt:    now,
			DeadlineAt:     deadline,
		}
		if err := tx.Create(&attempt).Error; err != nil {
			return err
		}

		// Update run's final_attempt_id in memory/DB
		run.Status = StatusRunning
		run.Version = run.Version + 1
		run.FinalAttemptID = attempt.ID
		tx.Model(&DiagnosisRun{}).Where("id = ?", runID).Update("final_attempt_id", attempt.ID)

		claimedRun = run
		return nil
	})

	if err != nil {
		return nil, nil, err
	}
	return &claimedRun, &attempt, nil
}

func (s *GormStore) GetAttempt(ctx context.Context, attemptID string) (*DiagnosisAttempt, error) {
	var att DiagnosisAttempt
	if err := s.db.WithContext(ctx).First(&att, "id = ?", attemptID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrAttemptNotFound
		}
		return nil, err
	}
	return &att, nil
}

func (s *GormStore) ListAttemptsByRun(ctx context.Context, runID string) ([]DiagnosisAttempt, error) {
	var attempts []DiagnosisAttempt
	err := s.db.WithContext(ctx).Where("diagnosis_run_id = ?", runID).Order("attempt_no ASC").Find(&attempts).Error
	return attempts, err
}

func (s *GormStore) UpdateAttemptHeartbeat(ctx context.Context, attemptID string, heartbeatAt time.Time) error {
	return s.db.WithContext(ctx).Model(&DiagnosisAttempt{}).
		Where("id = ? AND status = ?", attemptID, AttemptStatusRunning).
		Update("heartbeat_at", heartbeatAt).Error
}

func (s *GormStore) FinishAttemptAndRun(ctx context.Context, runID, attemptID string, newRunStatus RunStatus, newAttemptStatus AttemptStatus, promptTokens, completionTokens, toolCalls int, errCode, errMsg string, retryable bool, retryDelay time.Duration) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()

		// Update attempt
		attemptUpdates := map[string]interface{}{
			"status":            newAttemptStatus,
			"finished_at":       &now,
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"tool_calls":        toolCalls,
			"error_code":        errCode,
			"error_message":     errMsg,
			"retryable":         retryable,
		}
		resAtt := tx.Model(&DiagnosisAttempt{}).
			Where("id = ? AND status = ?", attemptID, AttemptStatusRunning).
			Updates(attemptUpdates)
		if resAtt.Error != nil {
			return resAtt.Error
		}

		// Update run
		runUpdates := map[string]interface{}{
			"status":           newRunStatus,
			"final_attempt_id": attemptID,
			"version":          gorm.Expr("version + 1"),
		}
		resRun := tx.Model(&DiagnosisRun{}).
			Where("id = ?", runID).
			Updates(runUpdates)
		if resRun.Error != nil {
			return resRun.Error
		}

		// If retryable failure, create OutboxEvent for delayed retry
		if newRunStatus == StatusRetryWait {
			availableAt := now.Add(retryDelay)
			payload, _ := json.Marshal(map[string]interface{}{
				"diagnosis_run_id": runID,
				"retry_count":      1,
			})
			evt := &outbox.OutboxEvent{
				ID:            uuid.New().String(),
				AggregateType: outbox.AggregateDiagnosisRun,
				AggregateID:   runID,
				EventType:     outbox.EventDiagnosisRetryRequested,
				Payload:       string(payload),
				Status:        outbox.StatusPending,
				AvailableAt:   availableAt,
			}
			if err := tx.Create(evt).Error; err != nil {
				return err
			}
		}

		return nil
	})
}

func (s *GormStore) RequestCancellation(ctx context.Context, runID, userID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run DiagnosisRun
		if err := tx.First(&run, "id = ? AND user_id = ?", runID, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRunNotFound
			}
			return err
		}

		if run.Status == StatusSucceeded || run.Status == StatusFailed || run.Status == StatusCancelled {
			return fmt.Errorf("cannot cancel run in terminal status %s", run.Status)
		}

		nextStatus := StatusCancelRequested
		if run.Status == StatusQueued {
			// If still queued, can transition directly to CANCELLED
			nextStatus = StatusCancelled
		}

		return tx.Model(&DiagnosisRun{}).
			Where("id = ?", runID).
			Updates(map[string]interface{}{
				"cancel_requested": true,
				"status":           nextStatus,
				"version":          gorm.Expr("version + 1"),
			}).Error
	})
}

func (s *GormStore) ConfirmCancellation(ctx context.Context, runID, attemptID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		if attemptID != "" {
			tx.Model(&DiagnosisAttempt{}).
				Where("id = ?", attemptID).
				Updates(map[string]interface{}{
					"status":      AttemptStatusCancelled,
					"finished_at": &now,
				})
		}
		return tx.Model(&DiagnosisRun{}).
			Where("id = ?", runID).
			Updates(map[string]interface{}{
				"status":  StatusCancelled,
				"version": gorm.Expr("version + 1"),
			}).Error
	})
}

func (s *GormStore) FetchStaleAttempts(ctx context.Context, staleDuration time.Duration, limit int) ([]DiagnosisAttempt, error) {
	if limit <= 0 {
		limit = 50
	}
	cutoff := time.Now().Add(-staleDuration)
	now := time.Now()

	var attempts []DiagnosisAttempt
	err := s.db.WithContext(ctx).
		Where("status = ? AND (heartbeat_at < ? OR deadline_at < ?)", AttemptStatusRunning, cutoff, now).
		Limit(limit).
		Find(&attempts).Error
	return attempts, err
}

func (s *GormStore) RecoverStaleAttempt(ctx context.Context, attemptID, runID string, backoff time.Duration) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()

		// Mark Attempt as ABANDONED
		resAtt := tx.Model(&DiagnosisAttempt{}).
			Where("id = ? AND status = ?", attemptID, AttemptStatusRunning).
			Updates(map[string]interface{}{
				"status":        AttemptStatusAbandoned,
				"finished_at":   &now,
				"error_code":    "WORKER_CRASH_OR_STALE",
				"error_message": "Worker crashed or lost ownership, attempt abandoned",
			})
		if resAtt.Error != nil {
			return resAtt.Error
		}
		if resAtt.RowsAffected == 0 {
			// Already moved
			return nil
		}

		// Transition Run to RETRY_WAIT
		resRun := tx.Model(&DiagnosisRun{}).
			Where("id = ? AND status = ?", runID, StatusRunning).
			Updates(map[string]interface{}{
				"status":  StatusRetryWait,
				"version": gorm.Expr("version + 1"),
			})
		if resRun.Error != nil {
			return resRun.Error
		}

		// Create Outbox event for retry
		availableAt := now.Add(backoff)
		payload, _ := json.Marshal(map[string]interface{}{
			"diagnosis_run_id": runID,
			"recovery_reason":  "stale_attempt_recovered",
		})
		evt := &outbox.OutboxEvent{
			ID:            uuid.New().String(),
			AggregateType: outbox.AggregateDiagnosisRun,
			AggregateID:   runID,
			EventType:     outbox.EventDiagnosisRetryRequested,
			Payload:       string(payload),
			Status:        outbox.StatusPending,
			AvailableAt:   availableAt,
		}
		return tx.Create(evt).Error
	})
}
