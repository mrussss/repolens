package diagnosis

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"repolens/internal/evidence"
	"repolens/internal/jobs"
)

var (
	ErrIdempotencyConflict     = errors.New("idempotency conflict: request payload differs from existing record")
	ErrInvalidBuildSelection   = errors.New("code index and retrieval build IDs must be positive")
	ErrBuildNotReady           = errors.New("diagnosis build is not ready")
	ErrProviderNotConfigured   = errors.New("provider is not configured")
	ErrProviderIdentityChanged = errors.New("provider identity changed since diagnosis was created")
	ErrRunNotFound             = errors.New("diagnosis run not found")
	ErrAttemptNotFound         = errors.New("diagnosis attempt not found")
	ErrAttemptNotRunning       = errors.New("diagnosis attempt is not running")
	ErrAttemptAlreadyExists    = errors.New("diagnosis attempt already exists")
	ErrAttemptLeaseActive      = errors.New("diagnosis attempt still has an active job lease")
	ErrClaimConflict           = errors.New("run claim conflict: status is not in expected state or already claimed")
	ErrRunTransitionConflict   = errors.New("diagnosis run is not running")
	ErrOptimisticLock          = errors.New("optimistic lock conflict")
)

type Store interface {
	Create(ctx context.Context, run *DiagnosisRun) error
	GetByID(ctx context.Context, id string) (*DiagnosisRun, error)
	GetByIDAndUser(ctx context.Context, id, userID string) (*DiagnosisRun, error)
	GetByIdempotencyKey(ctx context.Context, userID, key string) (*DiagnosisRun, error)
	ListByUser(ctx context.Context, userID string, page, pageSize int) ([]DiagnosisRun, int64, error)
	HasActiveRuns(ctx context.Context) (bool, error)
	WithProviderConfigLock(ctx context.Context, fn func() error) error
	ClaimRun(ctx context.Context, runID string, expectedStatuses []RunStatus, workerID string, attemptDeadline time.Duration) (*DiagnosisRun, *DiagnosisAttempt, error)
	GetAttempt(ctx context.Context, attemptID string) (*DiagnosisAttempt, error)
	GetLatestFinalCheckpoint(ctx context.Context, runID string, executionGeneration int) (*DiagnosisAttempt, error)
	ListAttemptsByRun(ctx context.Context, runID string) ([]DiagnosisAttempt, error)
	UpdateAttemptHeartbeat(ctx context.Context, attemptID string, heartbeatAt time.Time) error
	CloseAttempt(ctx context.Context, runID, attemptID string, generation int, newStatus AttemptStatus, errCode, errMsg string, retryable bool) error
	FinishAttempt(ctx context.Context, runID, attemptID string, newAttemptStatus AttemptStatus, promptTokens, completionTokens, toolCalls int, errCode, errMsg string, retryable bool) error
	FinishAttemptAndRun(ctx context.Context, runID, attemptID string, newRunStatus RunStatus, newAttemptStatus AttemptStatus, promptTokens, completionTokens, toolCalls int, errCode, errMsg string, retryable bool, retryDelay time.Duration) error
	RequestCancellation(ctx context.Context, runID, userID string) error
	ConfirmCancellation(ctx context.Context, runID, attemptID string) error
	FetchStaleAttempts(ctx context.Context, staleDuration time.Duration, limit int) ([]DiagnosisAttempt, error)
	RecoverStaleAttempt(ctx context.Context, attemptID, runID string, backoff time.Duration) error
}

type GormStore struct {
	db               *gorm.DB
	providerConfigMu sync.Mutex
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
		var existing DiagnosisAttempt
		err := tx.Where("id = ?", attempt.ID).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if attempt.StartedAt.IsZero() {
				attempt.StartedAt = time.Now().UTC()
			}
			if attempt.HeartbeatAt.IsZero() {
				attempt.HeartbeatAt = attempt.StartedAt
			}
			if attempt.DeadlineAt.IsZero() {
				attempt.DeadlineAt = attempt.StartedAt.Add(30 * time.Minute)
			}
			if attempt.ExecutionGeneration <= 0 {
				attempt.ExecutionGeneration = 1
			}
			if attempt.CheckpointKind == "" {
				attempt.CheckpointKind = CheckpointKindNone
			}
			attempt.Status = AttemptStatusRunning
			return tx.Create(attempt).Error
		}
		if err != nil {
			return err
		}
		return ErrAttemptAlreadyExists
	})
}

// FinalizeSuccess atomically persists report/citations, closes the attempt and
// diagnosis, and fences the AnalysisJob by worker and claim token.
func (s *GormStore) FinalizeSuccess(ctx context.Context, jobID int64, workerID, claimToken, runID, attemptID string, report *evidence.Report, citations []evidence.Citation, promptTokens, completionTokens, toolCalls int) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job jobs.AnalysisJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ?", jobID, jobs.StatusRunning, workerID, claimToken).First(&job).Error; err != nil {
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
			Updates(map[string]interface{}{
				"status": AttemptStatusSucceeded, "finished_at": now,
				"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "tool_calls": toolCalls,
				"raw_output":              report.RawOutput,
				"structured_output_valid": report.ReportStatus != evidence.ReportInvalid,
				"provider_completed_at":   now,
			})
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

// FinalizeInvalidStructuredReport atomically records an invalid model report
// and terminalizes the diagnosis attempt, run, and claimed AnalysisJob. The
// claim and cancellation checks are repeated inside the transaction so a
// stale worker can never overwrite a newer attempt or a user cancellation.
func (s *GormStore) FinalizeInvalidStructuredReport(ctx context.Context, jobID int64, workerID, claimToken, runID, attemptID string, report *evidence.Report, promptTokens, completionTokens, toolCalls int, errorCode, errorMessage string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job jobs.AnalysisJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ?", jobID, jobs.StatusRunning, workerID, claimToken).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				var current jobs.AnalysisJob
				if lookupErr := tx.Select("status").First(&current, jobID).Error; lookupErr == nil && (current.Status == jobs.StatusFailed || current.Status == jobs.StatusSucceeded || current.Status == jobs.StatusCancelled) {
					return jobs.ErrAlreadyFinalized
				}
				return jobs.ErrOwnershipLost
			}
			return err
		}
		if job.CancelRequested {
			return jobs.ErrCancellationRequested
		}
		if report == nil {
			return errors.New("invalid structured report is required")
		}
		if report.DiagnosisRunID != runID || report.AttemptID != attemptID {
			return errors.New("report lineage does not match diagnosis attempt")
		}

		var run DiagnosisRun
		if err := tx.Where("id = ? AND status = ?", runID, StatusRunning).First(&run).Error; err != nil {
			return err
		}
		if run.CancelRequested {
			return jobs.ErrCancellationRequested
		}
		var attempt DiagnosisAttempt
		if err := tx.Where("id = ? AND diagnosis_run_id = ? AND status = ?", attemptID, runID, AttemptStatusRunning).First(&attempt).Error; err != nil {
			return err
		}

		report.ReportStatus = evidence.ReportInvalid
		if report.ID == "" {
			report.ID = uuid.New().String()
		}
		if report.FindingsJSON == "" {
			report.FindingsJSON = "[]"
		}
		if report.RecommendedChecksJSON == "" {
			report.RecommendedChecksJSON = "[]"
		}
		if report.StructuredPayloadJSON == "" {
			report.StructuredPayloadJSON = "{}"
		}
		if report.LimitationsJSON == "" {
			report.LimitationsJSON = "[]"
		}
		if report.ParseError == "" {
			report.ParseError = errorMessage
		}
		if err := tx.Create(report).Error; err != nil {
			return fmt.Errorf("persist invalid report: %w", err)
		}

		now := time.Now().UTC()
		attemptResult := tx.Model(&DiagnosisAttempt{}).
			Where("id = ? AND diagnosis_run_id = ? AND status = ?", attemptID, runID, AttemptStatusRunning).
			Updates(map[string]interface{}{
				"status": AttemptStatusFailedTerminal, "finished_at": now,
				"prompt_tokens": promptTokens, "completion_tokens": completionTokens, "tool_calls": toolCalls,
				"error_code": errorCode, "error_message": errorMessage, "retryable": false,
				"raw_output": report.RawOutput, "structured_output_valid": false,
			})
		if attemptResult.Error != nil {
			return fmt.Errorf("finalize invalid attempt: %w", attemptResult.Error)
		}
		if attemptResult.RowsAffected != 1 {
			return fmt.Errorf("attempt %s finalize conflict", attemptID)
		}

		runResult := tx.Model(&DiagnosisRun{}).Where("id = ? AND status = ? AND cancel_requested = ?", runID, StatusRunning, false).
			Updates(map[string]interface{}{"status": StatusFailed, "final_attempt_id": attemptID, "version": gorm.Expr("version + 1")})
		if runResult.Error != nil {
			return fmt.Errorf("finalize invalid diagnosis: %w", runResult.Error)
		}
		if runResult.RowsAffected != 1 {
			return fmt.Errorf("diagnosis %s finalize conflict", runID)
		}

		jobResult := tx.Model(&jobs.AnalysisJob{}).
			Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ? AND cancel_requested = ?", jobID, jobs.StatusRunning, workerID, claimToken, false).
			Updates(map[string]interface{}{
				"status": jobs.StatusFailed, "terminal_reason": jobs.TerminalReasonPermanent,
				"last_error_class": jobs.ErrorClassPermanent, "last_error_code": errorCode, "last_error_message": errorMessage,
				"finished_at": now, "updated_at": now,
			})
		if jobResult.Error != nil {
			return fmt.Errorf("finalize invalid job: %w", jobResult.Error)
		}
		if jobResult.RowsAffected != 1 {
			return jobs.ErrOwnershipLost
		}
		return nil
	})
}

func (s *GormStore) FinalizeCancellation(ctx context.Context, jobID int64, workerID, claimToken, runID, attemptID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var job jobs.AnalysisJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND status = ? AND worker_id = ? AND claim_token = ?", jobID, jobs.StatusRunning, workerID, claimToken).First(&job).Error; err != nil {
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
			"status": StatusCancelled, "final_attempt_id": attemptID, "version": gorm.Expr("version + 1"),
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

// HasActiveRuns reports whether any user has a queued or running diagnosis.
// The EXISTS query is independent of pagination and does not load run data.
func (s *GormStore) HasActiveRuns(ctx context.Context) (bool, error) {
	var active bool
	err := s.db.WithContext(ctx).Raw(
		"SELECT EXISTS (SELECT 1 FROM diagnosis_runs WHERE status IN (?, ?))",
		StatusQueued, StatusRunning,
	).Scan(&active).Error
	return active, err
}

// WithProviderConfigLock serializes provider-identity changes with diagnosis
// creation. MySQL's named lock coordinates API processes; SQLite and other
// local test/development stores use the shared GormStore instance mutex.
func (s *GormStore) WithProviderConfigLock(ctx context.Context, fn func() error) error {
	if fn == nil {
		return nil
	}
	if s.db.Dialector.Name() != "mysql" {
		s.providerConfigMu.Lock()
		defer s.providerConfigMu.Unlock()
		return fn()
	}

	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", "repolens:provider_config_identity", 30).Scan(&acquired); err != nil {
		return fmt.Errorf("acquire provider identity lock: %w", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return errors.New("timed out acquiring provider identity lock")
	}
	defer func() {
		var released sql.NullInt64
		_ = conn.QueryRowContext(context.Background(), "SELECT RELEASE_LOCK(?)", "repolens:provider_config_identity").Scan(&released)
	}()
	return fn()
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

		generation := 1
		var job jobs.AnalysisJob
		jobErr := tx.Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, runID).First(&job).Error
		if jobErr != nil && !errors.Is(jobErr, gorm.ErrRecordNotFound) {
			return jobErr
		}
		if jobErr == nil && job.ExecutionGeneration > 0 {
			generation = job.ExecutionGeneration
		}

		// Number attempts within the active execution generation.
		var count int64
		if err := tx.Model(&DiagnosisAttempt{}).Where("diagnosis_run_id = ? AND execution_generation = ?", runID, generation).Count(&count).Error; err != nil {
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
			ID:                  uuid.New().String(),
			DiagnosisRunID:      runID,
			ExecutionGeneration: generation,
			AttemptNo:           int(count) + 1,
			WorkerID:            workerID,
			Status:              AttemptStatusRunning,
			StartedAt:           now,
			HeartbeatAt:         now,
			DeadlineAt:          deadline,
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

func (s *GormStore) GetLatestFinalCheckpoint(ctx context.Context, runID string, executionGeneration int) (*DiagnosisAttempt, error) {
	var attempt DiagnosisAttempt
	err := s.db.WithContext(ctx).
		Where("diagnosis_run_id = ? AND execution_generation = ? AND checkpoint_kind IN ? AND provider_completed_at IS NOT NULL", runID, executionGeneration, []CheckpointKind{CheckpointKindFinalValid, CheckpointKindFinalInvalid}).
		Order("attempt_no DESC, created_at DESC").
		First(&attempt).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &attempt, nil
}

// UpdateAttemptCheckpoint persists provider output before citation validation
// or the final business transaction. A later worker retry can inspect a final
// checkpoint instead of calling the provider again.
func (s *GormStore) UpdateAttemptCheckpoint(ctx context.Context, attemptID string, checkpoint AttemptCheckpoint) error {
	return s.updateAttemptCheckpoint(ctx, attemptID, checkpoint, false)
}

// UpdateAttemptCheckpointWithDraft persists the parsed model draft alongside
// the resolved compatibility report. A retry can therefore finish persistence
// without calling the provider again.
func (s *GormStore) UpdateAttemptCheckpointWithDraft(ctx context.Context, attemptID string, checkpoint AttemptCheckpoint) error {
	return s.updateAttemptCheckpoint(ctx, attemptID, checkpoint, true)
}

func (s *GormStore) updateAttemptCheckpoint(ctx context.Context, attemptID string, checkpoint AttemptCheckpoint, withDraft bool) error {
	if checkpoint.ExecutionGeneration <= 0 {
		checkpoint.ExecutionGeneration = 1
	}
	if checkpoint.Kind == "" {
		checkpoint.Kind = CheckpointKindNone
	}
	updates := map[string]interface{}{
		"checkpoint_kind":           checkpoint.Kind,
		"checkpoint_error_code":     checkpoint.ErrorCode,
		"checkpoint_error_message":  truncateCheckpointMessage(checkpoint.ErrorMessage),
		"raw_output":                checkpoint.RawOutput,
		"parsed_report_json":        checkpoint.ParsedReportJSON,
		"checkpoint_prompt_version": checkpoint.PromptVersion,
		"checkpoint_agent_version":  checkpoint.AgentVersion,
		"structured_output_valid":   checkpoint.Structured,
		"prompt_tokens":             checkpoint.PromptTokens,
		"completion_tokens":         checkpoint.CompletionTokens,
		"cached_prompt_tokens":      checkpoint.CachedPromptTokens,
		"reasoning_tokens":          checkpoint.ReasoningTokens,
		"tool_calls":                checkpoint.ToolCalls,
		"agent_rounds":              checkpoint.AgentRounds,
		"search_calls":              checkpoint.SearchCalls,
		"provider_calls":            checkpoint.ProviderCalls,
		"finalization_reason":       checkpoint.FinalizationReason,
		"finish_reason":             checkpoint.FinishReason,
		"provider_completed_at":     nil,
	}
	if withDraft {
		updates["parsed_report_draft_json"] = checkpoint.ParsedDraftJSON
	}
	if checkpoint.Kind == CheckpointKindFinalValid || checkpoint.Kind == CheckpointKindFinalInvalid {
		updates["provider_completed_at"] = time.Now().UTC()
	}
	result := s.db.WithContext(ctx).Model(&DiagnosisAttempt{}).
		Where("id = ? AND execution_generation = ? AND status = ?", attemptID, checkpoint.ExecutionGeneration, AttemptStatusRunning).
		Updates(updates)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrAttemptNotRunning
	}
	return nil
}

func truncateCheckpointMessage(message string) string {
	runes := []rune(message)
	if len(runes) > 255 {
		return string(runes[:255])
	}
	return message
}

func (s *GormStore) ListAttemptsByRun(ctx context.Context, runID string) ([]DiagnosisAttempt, error) {
	var attempts []DiagnosisAttempt
	err := s.db.WithContext(ctx).Where("diagnosis_run_id = ?", runID).
		Order("execution_generation ASC, attempt_no ASC, created_at ASC").Find(&attempts).Error
	return attempts, err
}

func (s *GormStore) UpdateAttemptHeartbeat(ctx context.Context, attemptID string, heartbeatAt time.Time) error {
	result := s.db.WithContext(ctx).Model(&DiagnosisAttempt{}).
		Where("id = ? AND status = ?", attemptID, AttemptStatusRunning).
		Update("heartbeat_at", heartbeatAt)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrAttemptNotRunning
	}
	return nil
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

// CloseAttempt transitions one RUNNING Attempt without changing its Run. It is
// used when a worker has lost finalization ownership: the stale Attempt still
// needs a terminal status, but must never move a Run that another worker has
// already finalized. The generation predicate and RowsAffected check make the
// close operation safe to race with another finalizer or the recovery sweeper.
func (s *GormStore) CloseAttempt(ctx context.Context, runID, attemptID string, generation int, newStatus AttemptStatus, errCode, errMsg string, retryable bool) error {
	if newStatus != AttemptStatusFailedRetryable && newStatus != AttemptStatusFailedTerminal && newStatus != AttemptStatusCancelled && newStatus != AttemptStatusAbandoned {
		return fmt.Errorf("invalid closed Attempt status %q", newStatus)
	}
	now := time.Now().UTC()
	result := s.db.WithContext(ctx).Model(&DiagnosisAttempt{}).
		Where("id = ? AND diagnosis_run_id = ? AND execution_generation = ? AND status = ?", attemptID, runID, generation, AttemptStatusRunning).
		Updates(map[string]interface{}{
			"status": newStatus, "finished_at": &now, "error_code": errCode,
			"error_message": errMsg, "retryable": retryable,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrAttemptNotRunning
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
			Where("id = ? AND diagnosis_run_id = ? AND status = ?", attemptID, runID, AttemptStatusRunning).
			Updates(attemptUpdates)
		if resAtt.Error != nil {
			return resAtt.Error
		}
		if resAtt.RowsAffected != 1 {
			return ErrAttemptNotRunning
		}

		// Update run
		runUpdates := map[string]interface{}{
			"status":           newRunStatus,
			"final_attempt_id": attemptID,
			"version":          gorm.Expr("version + 1"),
		}
		runQuery := tx.Model(&DiagnosisRun{}).Where("id = ? AND status = ?", runID, StatusRunning)
		if newRunStatus != StatusCancelled {
			runQuery = runQuery.Where("cancel_requested = ?", false)
		}
		resRun := runQuery.Updates(runUpdates)
		if resRun.Error != nil {
			return resRun.Error
		}
		if resRun.RowsAffected != 1 {
			return ErrRunTransitionConflict
		}

		return nil
	})
}

func (s *GormStore) RequestCancellation(ctx context.Context, runID, userID string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Lock Job before Run to match all claim-fenced diagnosis finalizers. A
		// retry finalizer and cancellation therefore serialize on the Job row:
		// cancellation either flags a RUNNING owner, or observes RETRY_WAIT and
		// terminalizes both state machines in this transaction.
		var job jobs.AnalysisJob
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, runID).First(&job).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("diagnosis job %s not found", runID)
			}
			return err
		}
		var run DiagnosisRun
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&run, "id = ? AND user_id = ?", runID, userID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrRunNotFound
			}
			return err
		}

		if run.Status == StatusSucceeded || run.Status == StatusFailed || run.Status == StatusCancelled {
			return fmt.Errorf("cannot cancel run in terminal status %s", run.Status)
		}

		nextStatus := run.Status
		jobUpdates := map[string]interface{}{"cancel_requested": true, "updated_at": time.Now().UTC()}
		jobTerminal := false
		switch run.Status {
		case StatusQueued:
			if job.Status == jobs.StatusPending || job.Status == jobs.StatusRetryWait {
				nextStatus = StatusCancelled
				jobTerminal = true
			} else if job.Status == jobs.StatusRunning {
				// A worker may have claimed the Job but not yet started its
				// DiagnosisAttempt. Let the claim owner observe cancellation.
			} else if job.Status == jobs.StatusCancelled {
				nextStatus = StatusCancelled
				jobTerminal = true
			} else {
				return fmt.Errorf("cannot cancel queued diagnosis with job status %s", job.Status)
			}
		case StatusRunning:
			switch job.Status {
			case jobs.StatusRunning:
				// The active owner will stop through the existing cancellation
				// poll and atomically finalize its Attempt, Run, and Job.
			case jobs.StatusRetryWait, jobs.StatusCancelled:
				// RETRY_WAIT has no active owner. Do not leave a RUNNING Run for
				// the generic pre-execution cancellation path to strand.
				nextStatus = StatusCancelled
				jobTerminal = true
			default:
				return fmt.Errorf("cannot cancel running diagnosis with job status %s", job.Status)
			}
		default:
			return fmt.Errorf("cannot cancel run in status %s", run.Status)
		}
		if jobTerminal {
			jobUpdates["status"] = jobs.StatusCancelled
			jobUpdates["terminal_reason"] = jobs.TerminalReasonCancelled
			jobUpdates["finished_at"] = time.Now().UTC()
		}
		jobRes := tx.Model(&jobs.AnalysisJob{}).Where("id = ? AND status = ?", job.ID, job.Status).Updates(jobUpdates)
		if jobRes.Error != nil {
			return jobRes.Error
		}
		if jobRes.RowsAffected != 1 {
			return fmt.Errorf("diagnosis job %s changed during cancellation", runID)
		}

		runRes := tx.Model(&DiagnosisRun{}).
			Where("id = ? AND user_id = ? AND status = ?", runID, userID, run.Status).
			Updates(map[string]interface{}{
				"cancel_requested": true,
				"status":           nextStatus,
				"version":          gorm.Expr("version + 1"),
			})
		if runRes.Error != nil {
			return runRes.Error
		}
		if runRes.RowsAffected != 1 {
			return fmt.Errorf("diagnosis run %s changed during cancellation", runID)
		}
		return nil
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
		var attempt DiagnosisAttempt
		if err := tx.Where("id = ? AND diagnosis_run_id = ? AND status = ?", attemptID, runID, AttemptStatusRunning).First(&attempt).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		// A stale Attempt heartbeat alone is not enough to prove the worker is
		// dead. A live lease protects only the Attempt represented by the current
		// job claim; a later automatic retry in the same generation must not keep
		// an older crashed Attempt alive.
		var job jobs.AnalysisJob
		jobErr := tx.Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, runID).First(&job).Error
		if jobErr != nil && !errors.Is(jobErr, gorm.ErrRecordNotFound) {
			return jobErr
		}
		jobOwnsAttempt := jobErr == nil && job.Status == jobs.StatusRunning &&
			job.ExecutionGeneration == attempt.ExecutionGeneration && job.AttemptCount == attempt.AttemptNo &&
			job.WorkerID != nil && *job.WorkerID == attempt.WorkerID &&
			job.LeaseUntil != nil && job.LeaseUntil.After(now)
		if jobOwnsAttempt {
			return ErrAttemptLeaseActive
		}

		// Mark Attempt as ABANDONED
		resAtt := tx.Model(&DiagnosisAttempt{}).
			Where("id = ? AND diagnosis_run_id = ? AND execution_generation = ? AND status = ?", attemptID, runID, attempt.ExecutionGeneration, AttemptStatusRunning).
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
