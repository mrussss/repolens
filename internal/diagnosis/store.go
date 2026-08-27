package diagnosis

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"repolens/internal/evidence"
	"repolens/internal/jobs"
)

var (
	ErrIdempotencyConflict = errors.New("idempotency conflict: request payload differs from existing record")
	ErrRunNotFound         = errors.New("diagnosis run not found")
	ErrAttemptNotFound     = errors.New("diagnosis attempt not found")
	ErrClaimConflict       = errors.New("run claim conflict: status is not in expected state or already claimed")
	ErrOptimisticLock      = errors.New("optimistic lock conflict")
)

type Store interface {
	Create(ctx context.Context, run *DiagnosisRun) error
	GetByID(ctx context.Context, id string) (*DiagnosisRun, error)
	GetByIDAndUser(ctx context.Context, id, userID string) (*DiagnosisRun, error)
	GetByIdempotencyKey(ctx context.Context, userID, key string) (*DiagnosisRun, error)
	ListByUser(ctx context.Context, userID string, page, pageSize int) ([]DiagnosisRun, int64, error)
	ClaimRun(ctx context.Context, runID string, expectedStatuses []RunStatus, workerID string, attemptDeadline time.Duration) (*DiagnosisRun, *DiagnosisAttempt, error)
	GetAttempt(ctx context.Context, attemptID string) (*DiagnosisAttempt, error)
	ListAttemptsByRun(ctx context.Context, runID string) ([]DiagnosisAttempt, error)
	UpdateAttemptHeartbeat(ctx context.Context, attemptID string, heartbeatAt time.Time) error
	FinishAttempt(ctx context.Context, runID, attemptID string, newAttemptStatus AttemptStatus, promptTokens, completionTokens, toolCalls int, errCode, errMsg string, retryable bool) error
	FinishAttemptAndRun(ctx context.Context, runID, attemptID string, newRunStatus RunStatus, newAttemptStatus AttemptStatus, promptTokens, completionTokens, toolCalls int, errCode, errMsg string, retryable bool, retryDelay time.Duration) error
	RequestCancellation(ctx context.Context, runID, userID string) error
	ConfirmCancellation(ctx context.Context, runID, attemptID string) error
	FetchStaleAttempts(ctx context.Context, staleDuration time.Duration, limit int) ([]DiagnosisAttempt, error)
	RecoverStaleAttempt(ctx context.Context, attemptID, runID string, backoff time.Duration) error
}

type GormStore struct {
	db *gorm.DB
}

// StartAttempt records the business transition and attempt row before an
// executor is called. It is deliberately separate from AnalysisJob claiming;
// the job store owns execution leases while this store owns diagnosis state.
func (s *GormStore) StartAttempt(ctx context.Context, runID string, attempt *DiagnosisAttempt) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run DiagnosisRun
		if err := tx.First(&run, "id = ?", runID).Error; err != nil {
			return err
		}
		if run.Status == StatusQueued {
			res := tx.Model(&DiagnosisRun{}).Where("id = ? AND status = ?", runID, StatusQueued).
				Updates(map[string]interface{}{"status": StatusRunning, "version": gorm.Expr("version + 1")})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return ErrClaimConflict
			}
		} else if run.Status != StatusRunning {
			return fmt.Errorf("diagnosis %s cannot start from %s", runID, run.Status)
		}
		if err := tx.Where("id = ?", attempt.ID).First(&DiagnosisAttempt{}).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			if attempt.StartedAt.IsZero() {
				attempt.StartedAt = time.Now().UTC()
			}
			if attempt.HeartbeatAt.IsZero() {
				attempt.HeartbeatAt = attempt.StartedAt
			}
			if attempt.DeadlineAt.IsZero() {
				attempt.DeadlineAt = attempt.StartedAt.Add(30 * time.Minute)
			}
			attempt.Status = AttemptStatusRunning
			return tx.Create(attempt).Error
		} else {
			return err
		}
	})
}

// FinalizeSuccess atomically persists report/citations, closes the attempt and
// diagnosis, and fences the AnalysisJob by worker and claim token.
func (s *GormStore) FinalizeSuccess(ctx context.Context, jobID int64, workerID, claimToken, runID, attemptID string, report *evidence.Report, citations []evidence.Citation, promptTokens, completionTokens, toolCalls int) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job jobs.AnalysisJob
		if err := tx.Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ?", jobID, jobs.StatusRunning, workerID, claimToken).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return jobs.ErrOwnershipLost
			}
			return err
		}
		if job.CancelRequested {
			return jobs.ErrCancellationRequested
		}
		if report == nil {
			return errors.New("diagnosis report is required")
		}
		var run DiagnosisRun
		if err := tx.First(&run, "id = ? AND status = ?", runID, StatusRunning).Error; err != nil {
			return err
		}
		if run.CancelRequested {
			return jobs.ErrCancellationRequested
		}
		if report.DiagnosisRunID != runID || report.AttemptID != attemptID {
			return errors.New("report lineage does not match diagnosis attempt")
		}
		for _, citation := range citations {
			if citation.SnapshotID != run.SnapshotID || (run.CodeIndexBuildID > 0 && citation.CodeIndexBuildID != run.CodeIndexBuildID) {
				return errors.New("citation lineage does not match diagnosis snapshot/build")
			}
		}
		if err := tx.Create(report).Error; err != nil {
			return fmt.Errorf("persist report: %w", err)
		}
		for i := range citations {
			if citations[i].ID == "" {
				citations[i].ID = uuid.New().String()
			}
			if err := tx.Create(&citations[i]).Error; err != nil {
				return fmt.Errorf("persist citation: %w", err)
			}
		}
		now := time.Now().UTC()
		attRes := tx.Model(&DiagnosisAttempt{}).Where("id = ? AND diagnosis_run_id = ? AND status = ?", attemptID, runID, AttemptStatusRunning).
			Updates(map[string]interface{}{"status": AttemptStatusSucceeded, "finished_at": now, "prompt_tokens": promptTokens, "completion_tokens": completionTokens, "tool_calls": toolCalls})
		if attRes.Error != nil {
			return attRes.Error
		}
		if attRes.RowsAffected != 1 {
			return fmt.Errorf("attempt %s finalize conflict", attemptID)
		}
		runRes := tx.Model(&DiagnosisRun{}).Where("id = ? AND status = ?", runID, StatusRunning).
			Updates(map[string]interface{}{"status": StatusSucceeded, "final_attempt_id": attemptID, "version": gorm.Expr("version + 1")})
		if runRes.Error != nil {
			return runRes.Error
		}
		if runRes.RowsAffected != 1 {
			return fmt.Errorf("diagnosis %s finalize conflict", runID)
		}
		jobRes := tx.Model(&jobs.AnalysisJob{}).Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, false).
			Updates(map[string]interface{}{"status": jobs.StatusSucceeded, "finished_at": now, "updated_at": now})
		if jobRes.Error != nil {
			return jobRes.Error
		}
		if jobRes.RowsAffected != 1 {
			return jobs.ErrOwnershipLost
		}
		return nil
	})
}

func (s *GormStore) FinalizeCancellation(ctx context.Context, jobID int64, workerID, claimToken, runID, attemptID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job jobs.AnalysisJob
		if err := tx.Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ?", jobID, jobs.StatusRunning, workerID, claimToken).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return jobs.ErrOwnershipLost
			}
			return err
		}
		attemptResult := tx.Model(&DiagnosisAttempt{}).Where("id = ? AND diagnosis_run_id = ? AND status = ?", attemptID, runID, AttemptStatusRunning).Updates(map[string]interface{}{
			"status": AttemptStatusCancelled, "finished_at": time.Now().UTC(), "error_code": "CANCELLED",
		})
		if attemptResult.Error != nil {
			return attemptResult.Error
		}
		if attemptResult.RowsAffected != 1 {
			return jobs.ErrOwnershipLost
		}
		runResult := tx.Model(&DiagnosisRun{}).Where("id = ? AND status = ?", runID, StatusRunning).Updates(map[string]interface{}{
			"status": StatusCancelled, "version": gorm.Expr("version + 1"),
		})
		if runResult.Error != nil {
			return runResult.Error
		}
		if runResult.RowsAffected != 1 {
			return fmt.Errorf("diagnosis %s cancellation conflict", runID)
		}
		jobResult := tx.Model(&jobs.AnalysisJob{}).Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ?", jobID, jobs.StatusRunning, workerID, claimToken).Updates(map[string]interface{}{
			"status": jobs.StatusCancelled, "terminal_reason": jobs.TerminalReasonCancelled, "finished_at": time.Now().UTC(),
		})
		if jobResult.Error != nil {
			return jobResult.Error
		}
		if jobResult.RowsAffected != 1 {
			return jobs.ErrOwnershipLost
		}
		return nil
	})
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) Create(ctx context.Context, run *DiagnosisRun) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if run.ID == "" {
			run.ID = uuid.New().String()
		}
		run.Status = StatusQueued
		run.Version = 1

		if err := tx.Create(run).Error; err != nil {
			return err
		}

		// Atomically insert analysis_job for DB-backed job execution
		job := &jobs.AnalysisJob{
			JobType:             jobs.JobTypeRunDiagnosis,
			ResourceID:          run.ID,
			Status:              jobs.StatusPending,
			ExecutionGeneration: 1,
			AttemptCount:        0,
			MaxAttempts:         3,
			NextRunAt:           time.Now().UTC(),
		}
		if err := tx.Create(job).Error; err != nil {
			return err
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

func (s *GormStore) FinishAttempt(ctx context.Context, runID, attemptID string, newAttemptStatus AttemptStatus, promptTokens, completionTokens, toolCalls int, errCode, errMsg string, retryable bool) error {
	now := time.Now().UTC()
	result := s.db.WithContext(ctx).Model(&DiagnosisAttempt{}).
		Where("id = ? AND diagnosis_run_id = ? AND status = ?", attemptID, runID, AttemptStatusRunning).
		Updates(map[string]interface{}{
			"status": newAttemptStatus, "finished_at": &now,
			"prompt_tokens": promptTokens, "completion_tokens": completionTokens,
			"tool_calls": toolCalls, "error_code": errCode, "error_message": errMsg,
			"retryable": retryable,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrAttemptNotFound
	}
	return nil
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

		nextStatus := run.Status
		if run.Status == StatusQueued {
			// A queued diagnosis and its pending/retry job are cancelled together,
			// before another claimant can observe them as runnable.
			jobRes := tx.Model(&jobs.AnalysisJob{}).
				Where("job_type = ? AND resource_id = ? AND status IN (?, ?)", jobs.JobTypeRunDiagnosis, runID, jobs.StatusPending, jobs.StatusRetryWait).
				Updates(map[string]interface{}{"status": jobs.StatusCancelled, "terminal_reason": jobs.TerminalReasonCancelled, "cancel_requested": true, "finished_at": time.Now().UTC()})
			if jobRes.Error != nil {
				return jobRes.Error
			}
			if jobRes.RowsAffected != 1 {
				return fmt.Errorf("diagnosis job %s is no longer queued", runID)
			}
			nextStatus = StatusCancelled
		} else if run.Status != StatusRunning {
			return fmt.Errorf("cannot cancel run in status %s", run.Status)
		} else {
			jobRes := tx.Model(&jobs.AnalysisJob{}).
				Where("job_type = ? AND resource_id = ? AND status = ?", jobs.JobTypeRunDiagnosis, runID, jobs.StatusRunning).
				Updates(map[string]interface{}{"cancel_requested": true})
			if jobRes.Error != nil {
				return jobRes.Error
			}
			if jobRes.RowsAffected != 1 {
				return fmt.Errorf("diagnosis job %s is not running", runID)
			}
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

		// Retry is represented only by AnalysisJob. The business diagnosis
		// remains RUNNING and is resumed by the next job claim.
		resRun := tx.Model(&DiagnosisRun{}).
			Where("id = ? AND status = ?", runID, StatusRunning).
			Updates(map[string]interface{}{
				"version": gorm.Expr("version + 1"),
			})
		if resRun.Error != nil {
			return resRun.Error
		}

		return nil
	})
}
