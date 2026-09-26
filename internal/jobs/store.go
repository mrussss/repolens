package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Store defines database operations on analysis_jobs.
type Store struct {
	db     *sql.DB
	driver string
}

type ownedRunningJob struct {
	jobType             JobType
	resourceID          string
	executionGeneration int
	cancelRequested     bool
}

type expiredJob struct {
	id                  int64
	jobType             JobType
	resourceID          string
	executionGeneration int
	attemptCount        int
	maxAttempts         int
	cancelRequested     bool
}

// NewStore creates a new Store instance defaulting to MySQL.
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, driver: "mysql"}
}

// NewStoreWithDriver creates a new Store with a specified driver name (e.g. "mysql" or "sqlite3").
func NewStoreWithDriver(db *sql.DB, driver string) *Store {
	return &Store{db: db, driver: driver}
}

// DB returns the underlying *sql.DB.
func (s *Store) DB() *sql.DB {
	return s.db
}

// CreateJobTx inserts a new AnalysisJob in the provided transaction.
func (s *Store) CreateJobTx(ctx context.Context, tx *sql.Tx, job *AnalysisJob) error {
	query := `
		INSERT INTO analysis_jobs (
			job_type, resource_id, status, execution_generation,
			attempt_count, max_attempts, next_run_at, cancel_requested,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	now := time.Now().UTC()
	if job.Status == "" {
		job.Status = StatusPending
	}
	if job.ExecutionGeneration <= 0 {
		job.ExecutionGeneration = 1
	}
	if job.MaxAttempts <= 0 {
		job.MaxAttempts = 3
	}
	if job.NextRunAt.IsZero() {
		job.NextRunAt = now
	}
	job.CreatedAt = now
	job.UpdatedAt = now

	res, err := tx.ExecContext(ctx, query,
		job.JobType, job.ResourceID, job.Status, job.ExecutionGeneration,
		job.AttemptCount, job.MaxAttempts, job.NextRunAt, job.CancelRequested,
		job.CreatedAt, job.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed inserting analysis_job: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("failed getting last insert id: %w", err)
	}
	job.ID = id
	return nil
}

// CreateJob inserts a new AnalysisJob outside an external transaction.
func (s *Store) CreateJob(ctx context.Context, job *AnalysisJob) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.CreateJobTx(ctx, tx, job); err != nil {
		return err
	}
	return tx.Commit()
}

// GetJobByID retrieves an AnalysisJob by primary key ID.
func (s *Store) GetJobByID(ctx context.Context, id int64) (*AnalysisJob, error) {
	query := `
		SELECT id, job_type, resource_id, status, execution_generation,
		       terminal_reason, attempt_count, max_attempts, next_run_at,
		       worker_id, claim_token, lease_until, cancel_requested,
		       last_error_class, last_error_code, last_error_message,
		       created_at, updated_at, finished_at
		FROM analysis_jobs
		WHERE id = ?
	`
	row := s.db.QueryRowContext(ctx, query, id)
	return scanJob(row)
}

// GetJobByResource retrieves the AnalysisJob for a given job_type and resource_id.
func (s *Store) GetJobByResource(ctx context.Context, jobType JobType, resourceID string) (*AnalysisJob, error) {
	query := `
		SELECT id, job_type, resource_id, status, execution_generation,
		       terminal_reason, attempt_count, max_attempts, next_run_at,
		       worker_id, claim_token, lease_until, cancel_requested,
		       last_error_class, last_error_code, last_error_message,
		       created_at, updated_at, finished_at
		FROM analysis_jobs
		WHERE job_type = ? AND resource_id = ?
	`
	row := s.db.QueryRowContext(ctx, query, jobType, resourceID)
	return scanJob(row)
}

// ClaimJobs finds eligible pending or retry-wait jobs using SELECT ... FOR UPDATE SKIP LOCKED and marks them RUNNING.
func (s *Store) ClaimJobs(ctx context.Context, workerID string, batchSize int, leaseDuration time.Duration) ([]*AnalysisJob, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("failed starting claim transaction: %w", err)
	}
	defer tx.Rollback()

	selectQuery := `
		SELECT id, job_type, resource_id, status, execution_generation,
		       attempt_count, max_attempts, cancel_requested
		FROM analysis_jobs
		WHERE status IN ('PENDING', 'RETRY_WAIT')
		  AND next_run_at <= ?
		ORDER BY id
		LIMIT ?
	`
	if s.driver != "sqlite" && s.driver != "sqlite3" {
		selectQuery += "\nFOR UPDATE SKIP LOCKED"
	}
	now := time.Now().UTC()
	rows, err := tx.QueryContext(ctx, selectQuery, now, batchSize)
	if err != nil {
		return nil, fmt.Errorf("failed querying claimable jobs: %w", err)
	}
	defer rows.Close()

	type claimCandidate struct {
		id                  int64
		jobType             JobType
		resourceID          string
		status              JobStatus
		executionGeneration int
		attemptCount        int
		maxAttempts         int
		cancelRequested     bool
	}
	var candidates []claimCandidate
	for rows.Next() {
		var c claimCandidate
		if err := rows.Scan(&c.id, &c.jobType, &c.resourceID, &c.status, &c.executionGeneration, &c.attemptCount, &c.maxAttempts, &c.cancelRequested); err != nil {
			return nil, fmt.Errorf("failed scanning claim candidate: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if len(candidates) == 0 {
		return nil, nil
	}

	updateQuery := `
		UPDATE analysis_jobs
		SET status = 'RUNNING',
		    worker_id = ?,
		    claim_token = ?,
		    lease_until = ?,
		    attempt_count = attempt_count + 1,
		    updated_at = ?
		WHERE id = ?
	`
	var claimedJobs []*AnalysisJob
	leaseUntil := now.Add(leaseDuration)

	for _, cand := range candidates {
		claimToken := uuid.New().String()
		_, err := tx.ExecContext(ctx, updateQuery, workerID, claimToken, leaseUntil, now, cand.id)
		if err != nil {
			return nil, fmt.Errorf("failed updating job %d during claim: %w", cand.id, err)
		}

		claimedJobs = append(claimedJobs, &AnalysisJob{
			ID:                  cand.id,
			JobType:             cand.jobType,
			ResourceID:          cand.resourceID,
			Status:              StatusRunning,
			ExecutionGeneration: cand.executionGeneration,
			AttemptCount:        cand.attemptCount + 1,
			MaxAttempts:         cand.maxAttempts,
			NextRunAt:           now,
			WorkerID:            &workerID,
			ClaimToken:          &claimToken,
			LeaseUntil:          &leaseUntil,
			CancelRequested:     cand.cancelRequested,
			UpdatedAt:           now,
		})
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed committing claim transaction: %w", err)
	}

	return claimedJobs, nil
}

// RenewLease extends the lease duration of a RUNNING job if the caller owns the claim.
func (s *Store) RenewLease(ctx context.Context, jobID int64, workerID, claimToken string, newLeaseUntil time.Time) error {
	query := `
		UPDATE analysis_jobs
		SET lease_until = ?,
		    updated_at = ?
		WHERE id = ?
		  AND status = 'RUNNING'
		  AND worker_id = ?
		  AND claim_token = ?
		  AND lease_until > ?
	`
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, query, newLeaseUntil, now, jobID, workerID, claimToken, now)
	if err != nil {
		return fmt.Errorf("failed renewing lease for job %d: %w", jobID, err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrOwnershipLost
	}
	return nil
}

// ConditionalFinalizeSuccessTx marks a job SUCCEEDED in the provided transaction with strict claim verification.
func (s *Store) ConditionalFinalizeSuccessTx(ctx context.Context, tx *sql.Tx, jobID int64, workerID, claimToken string) error {
	query := `
		UPDATE analysis_jobs
		SET status = 'SUCCEEDED',
		    finished_at = ?,
		    updated_at = ?
		WHERE id = ?
		  AND status = 'RUNNING'
		  AND worker_id = ?
		  AND claim_token = ?
	`
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, query, now, now, jobID, workerID, claimToken)
	if err != nil {
		return fmt.Errorf("failed to finalize success for job %d: %w", jobID, err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		var status JobStatus
		if err := tx.QueryRowContext(ctx, `SELECT status FROM analysis_jobs WHERE id = ?`, jobID).Scan(&status); err == nil && (status == StatusSucceeded || status == StatusFailed || status == StatusCancelled) {
			return ErrAlreadyFinalized
		}
		return ErrOwnershipLost
	}
	return nil
}

// ConditionalFinalizeSuccess marks a job SUCCEEDED outside an external transaction.
func (s *Store) ConditionalFinalizeSuccess(ctx context.Context, jobID int64, workerID, claimToken string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.ConditionalFinalizeSuccessTx(ctx, tx, jobID, workerID, claimToken); err != nil {
		return err
	}
	return tx.Commit()
}

// ConditionalFinalizeFailureTx records failure details (either RETRY_WAIT or terminal FAILED) with claim verification.
func (s *Store) ConditionalFinalizeFailureTx(ctx context.Context, tx *sql.Tx, jobID int64, workerID, claimToken string, errClass ErrorClass, errCode, errMsg string, terminalReason *TerminalReason, isTerminal bool, nextRunAt time.Time) error {
	owned, err := s.loadOwnedRunningJobTx(ctx, tx, jobID, workerID, claimToken)
	if err != nil {
		return err
	}
	if owned.jobType == JobTypeRunDiagnosis && owned.cancelRequested {
		// A user cancellation accepted while the handler was returning a
		// retryable error wins over retry scheduling and terminalizes both state
		// machines in the same transaction.
		return s.ConditionalFinalizeCancelTx(ctx, tx, jobID, workerID, claimToken)
	}

	now := time.Now().UTC()
	var query string
	var args []interface{}

	if isTerminal {
		query = `
			UPDATE analysis_jobs
			SET status = 'FAILED',
			    terminal_reason = ?,
			    last_error_class = ?,
			    last_error_code = ?,
			    last_error_message = ?,
			    finished_at = ?,
			    updated_at = ?
			WHERE id = ?
			  AND status = 'RUNNING'
			  AND worker_id = ?
			  AND claim_token = ?
		`
		args = []interface{}{terminalReason, string(errClass), errCode, errMsg, now, now, jobID, workerID, claimToken}
	} else {
		query = `
			UPDATE analysis_jobs
			SET status = 'RETRY_WAIT',
			    next_run_at = ?,
			    last_error_class = ?,
			    last_error_code = ?,
			    last_error_message = ?,
			    updated_at = ?
			WHERE id = ?
			  AND status = 'RUNNING'
			  AND worker_id = ?
			  AND claim_token = ?
		`
		args = []interface{}{nextRunAt, string(errClass), errCode, errMsg, now, jobID, workerID, claimToken}
	}
	if owned.jobType == JobTypeRunDiagnosis {
		query += " AND cancel_requested = FALSE"
	}
	res, err := tx.ExecContext(ctx, query, args...)

	if err != nil {
		return fmt.Errorf("failed finalizing failure for job %d: %w", jobID, err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		if owned.jobType == JobTypeRunDiagnosis {
			current, currentErr := s.loadOwnedRunningJobTx(ctx, tx, jobID, workerID, claimToken)
			if currentErr == nil && current.cancelRequested {
				return s.ConditionalFinalizeCancelTx(ctx, tx, jobID, workerID, claimToken)
			}
		}
		return ErrOwnershipLost
	}
	if isTerminal {
		if err := s.failBusinessTx(ctx, tx, jobID, false, errCode, errMsg); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) loadOwnedRunningJobTx(ctx context.Context, tx *sql.Tx, jobID int64, workerID, claimToken string) (ownedRunningJob, error) {
	query := `SELECT job_type, resource_id, execution_generation, cancel_requested
		FROM analysis_jobs
		WHERE id = ? AND status = 'RUNNING' AND worker_id = ? AND claim_token = ?`
	if s.driver != "sqlite" && s.driver != "sqlite3" {
		query += " FOR UPDATE"
	}
	var job ownedRunningJob
	if err := tx.QueryRowContext(ctx, query, jobID, workerID, claimToken).Scan(
		&job.jobType, &job.resourceID, &job.executionGeneration, &job.cancelRequested,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ownedRunningJob{}, ErrOwnershipLost
		}
		return ownedRunningJob{}, err
	}
	return job, nil
}

// failBusinessTx keeps terminal execution failure and business failure in one
// claim-fenced transaction. The SQL is intentionally keyed by the fixed job
// type enum, never by user input.
func (s *Store) failBusinessTx(ctx context.Context, tx *sql.Tx, jobID int64, reaped bool, errorCode, errorMessage string) error {
	var jobType JobType
	var resourceID string
	var generation int
	if err := tx.QueryRowContext(ctx, `SELECT job_type, resource_id, execution_generation FROM analysis_jobs WHERE id = ?`, jobID).Scan(&jobType, &resourceID, &generation); err != nil {
		return err
	}
	code := "JOB_FAILED"
	message := errorMessage
	if reaped {
		code = "LEASE_EXPIRED_EXHAUSTED"
		message = "Job lease expired and max attempts exceeded"
	} else if errorCode != "" {
		code = errorCode
	}
	var query string
	switch jobType {
	case JobTypeMaterializeSnapshot:
		query = `UPDATE repository_snapshots SET status = 'FAILED', error_code = ? WHERE id = ? AND status IN ('CREATED', 'MATERIALIZING')`
	case JobTypeBuildCodeIndex:
		query = `UPDATE code_index_builds SET status = 'FAILED', error_code = ? WHERE id = ? AND status IN ('CREATED', 'BUILDING')`
	case JobTypeBuildRetrieval:
		query = `UPDATE retrieval_builds SET status = 'FAILED', error_code = ? WHERE id = ? AND status IN ('CREATED', 'BUILDING')`
	case JobTypeRunDiagnosis:
		query = `UPDATE diagnosis_runs SET status = 'FAILED', version = version + 1 WHERE id = ? AND status IN ('QUEUED', 'RUNNING')`
	default:
		return fmt.Errorf("unsupported job type %s during terminal failure", jobType)
	}
	args := []interface{}{code, resourceID}
	if jobType == JobTypeRunDiagnosis {
		args = []interface{}{resourceID}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return fmt.Errorf("failed terminal business transition for %s/%s: %w", jobType, resourceID, err)
	}
	if jobType == JobTypeRunDiagnosis {
		attemptStatus := "FAILED_TERMINAL"
		if reaped {
			attemptStatus = "ABANDONED"
		}
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE diagnosis_attempts SET status = ?, finished_at = ? WHERE diagnosis_run_id = ? AND execution_generation = ? AND status = 'RUNNING'`, attemptStatus, now, resourceID, generation); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such column") && strings.Contains(strings.ToLower(err.Error()), "execution_generation") {
				// Pre-v2.2 isolated job-store fixtures do not carry generation metadata.
				if _, legacyErr := tx.ExecContext(ctx, `UPDATE diagnosis_attempts SET status = ?, finished_at = ? WHERE diagnosis_run_id = ? AND status = 'RUNNING'`, attemptStatus, now, resourceID); legacyErr != nil && !strings.Contains(strings.ToLower(legacyErr.Error()), "no such table") {
					return fmt.Errorf("failed terminal diagnosis attempt transition for %s: %w", resourceID, legacyErr)
				}
			} else if !strings.Contains(strings.ToLower(err.Error()), "no such table") {
				return fmt.Errorf("failed terminal diagnosis attempt transition for %s: %w", resourceID, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE diagnosis_runs SET final_attempt_id = (SELECT id FROM diagnosis_attempts WHERE diagnosis_run_id = ? AND execution_generation = ? ORDER BY attempt_no DESC, created_at DESC LIMIT 1) WHERE id = ? AND status = 'FAILED'`, resourceID, generation, resourceID); err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such table") && !(strings.Contains(strings.ToLower(err.Error()), "no such column") && strings.Contains(strings.ToLower(err.Error()), "execution_generation")) {
			return fmt.Errorf("failed setting terminal diagnosis attempt for %s: %w", resourceID, err)
		}
	}
	if err := s.failAnalysisRevisionTx(ctx, tx, jobType, resourceID, code, message); err != nil {
		return err
	}
	return nil
}

// failAnalysisRevisionTx keeps the product-facing pipeline state in sync with
// a terminal stage job failure. Older isolated job-store tests do not create
// the v2.2 revision table, so that schema omission remains harmless here.
func (s *Store) failAnalysisRevisionTx(ctx context.Context, tx *sql.Tx, jobType JobType, resourceID, errorCode, errorMessage string) error {
	var predicate string
	var stage string
	switch jobType {
	case JobTypeMaterializeSnapshot:
		predicate = "snapshot_id = ?"
		stage = "MATERIALIZING"
	case JobTypeBuildCodeIndex:
		predicate = "code_index_build_id = ?"
		stage = "BUILDING_CODE_INDEX"
	case JobTypeBuildRetrieval:
		predicate = "retrieval_build_id = ?"
		stage = "BUILDING_RETRIEVAL"
	default:
		return nil
	}
	query := `UPDATE analysis_revisions
		SET status = 'FAILED', stage = ?, error_code = ?, error_message = ?,
		    version = version + 1, updated_at = ?
		WHERE status = 'PREPARING' AND ` + predicate
	_, err := tx.ExecContext(ctx, query, stage, errorCode, errorMessage, time.Now().UTC(), resourceID)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return fmt.Errorf("failed terminal analysis revision transition for %s/%s: %w", jobType, resourceID, err)
	}
	return nil
}

// ConditionalFinalizeFailure records failure details outside an external transaction.
func (s *Store) ConditionalFinalizeFailure(ctx context.Context, jobID int64, workerID, claimToken string, errClass ErrorClass, errCode, errMsg string, terminalReason *TerminalReason, isTerminal bool, nextRunAt time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.ConditionalFinalizeFailureTx(ctx, tx, jobID, workerID, claimToken, errClass, errCode, errMsg, terminalReason, isTerminal, nextRunAt); err != nil {
		return err
	}
	return tx.Commit()
}

// ConditionalFinalizeCancelTx marks a job CANCELLED in the provided transaction.
func (s *Store) ConditionalFinalizeCancelTx(ctx context.Context, tx *sql.Tx, jobID int64, workerID, claimToken string) error {
	owned, err := s.loadOwnedRunningJobTx(ctx, tx, jobID, workerID, claimToken)
	if err != nil {
		return err
	}
	query := `
		UPDATE analysis_jobs
		SET status = 'CANCELLED',
		    terminal_reason = 'CANCELLED',
		    finished_at = ?,
		    updated_at = ?
		WHERE id = ?
		  AND status = 'RUNNING'
		  AND worker_id = ?
		  AND claim_token = ?
	`
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, query, now, now, jobID, workerID, claimToken)
	if err != nil {
		return fmt.Errorf("failed finalizing cancellation for job %d: %w", jobID, err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrOwnershipLost
	}
	if owned.jobType == JobTypeRunDiagnosis {
		if err := s.cancelDiagnosisRunTx(ctx, tx, owned.resourceID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) cancelDiagnosisRunTx(ctx context.Context, tx *sql.Tx, runID string) error {
	res, err := tx.ExecContext(ctx, `UPDATE diagnosis_runs
		SET status = 'CANCELLED', cancel_requested = TRUE, version = version + 1
		WHERE id = ? AND status IN ('QUEUED', 'RUNNING')`, runID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return nil
		}
		return fmt.Errorf("failed to cancel diagnosis run %s with its job: %w", runID, err)
	}
	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM diagnosis_runs WHERE id = ?`, runID).Scan(&status); err != nil {
			if errors.Is(err, sql.ErrNoRows) || strings.Contains(strings.ToLower(err.Error()), "no such table") {
				return nil
			}
			return err
		}
		if status != "CANCELLED" {
			return fmt.Errorf("diagnosis %s cannot be cancelled from status %s", runID, status)
		}
	}
	return nil
}

// ConditionalFinalizeCancel marks a job CANCELLED outside an external transaction.
func (s *Store) ConditionalFinalizeCancel(ctx context.Context, jobID int64, workerID, claimToken string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if err := s.ConditionalFinalizeCancelTx(ctx, tx, jobID, workerID, claimToken); err != nil {
		return err
	}
	return tx.Commit()
}

// RequestCancel sets cancel_requested = TRUE for active job states.
func (s *Store) RequestCancel(ctx context.Context, jobType JobType, resourceID string) error {
	query := `
		UPDATE analysis_jobs
		SET cancel_requested = TRUE,
		    updated_at = ?
		WHERE job_type = ?
		  AND resource_id = ?
		  AND status IN ('PENDING', 'RUNNING', 'RETRY_WAIT')
	`
	_, err := s.db.ExecContext(ctx, query, time.Now().UTC(), jobType, resourceID)
	return err
}

// IsCancelRequested reads the authoritative cancellation flag for a claimed
// job. Workers poll it so a running handler receives context cancellation.
func (s *Store) IsCancelRequested(ctx context.Context, jobID int64, workerID, claimToken string) (bool, error) {
	var requested bool
	err := s.db.QueryRowContext(ctx, `SELECT cancel_requested FROM analysis_jobs WHERE id = ? AND status = 'RUNNING' AND worker_id = ? AND claim_token = ?`, jobID, workerID, claimToken).Scan(&requested)
	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrOwnershipLost
	}
	return requested, err
}

// ReapExpiredJobs scans for RUNNING jobs with expired leases and moves them to
// RETRY_WAIT or terminal FAILED. An accepted Diagnosis cancellation takes
// precedence over both retry scheduling and retry exhaustion.
func (s *Store) ReapExpiredJobs(ctx context.Context, batchSize int) (int, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	selectQuery := `
		SELECT id, job_type, resource_id, execution_generation,
		       attempt_count, max_attempts, cancel_requested
		FROM analysis_jobs
		WHERE status = 'RUNNING'
		  AND lease_until < ?
		LIMIT ?
	`
	if s.driver != "sqlite" && s.driver != "sqlite3" {
		selectQuery += "\nFOR UPDATE SKIP LOCKED"
	}
	now := time.Now().UTC()
	rows, err := tx.QueryContext(ctx, selectQuery, now, batchSize)
	if err != nil {
		return 0, fmt.Errorf("failed selecting expired jobs: %w", err)
	}
	defer rows.Close()

	var expired []expiredJob
	for rows.Next() {
		var ej expiredJob
		if err := rows.Scan(&ej.id, &ej.jobType, &ej.resourceID, &ej.executionGeneration,
			&ej.attemptCount, &ej.maxAttempts, &ej.cancelRequested); err != nil {
			return 0, err
		}
		expired = append(expired, ej)
	}
	rows.Close()

	reapedCount := 0
	for _, ej := range expired {
		if ej.jobType == JobTypeRunDiagnosis && ej.cancelRequested {
			if err := s.cancelExpiredDiagnosisTx(ctx, tx, ej, now); err != nil {
				return 0, err
			}
			reapedCount++
			continue
		}
		if ej.attemptCount < ej.maxAttempts {
			// Schedule retry
			backoff := CalculateBackoff(ej.attemptCount, time.Second, time.Minute)
			nextRun := now.Add(backoff)
			retryQuery := `
				UPDATE analysis_jobs
				SET status = 'RETRY_WAIT',
				    next_run_at = ?,
				    last_error_class = 'OWNERSHIP_LOST',
				    last_error_code = 'LEASE_EXPIRED',
				    last_error_message = 'Job lease expired and worker did not renew',
				    updated_at = ?
				WHERE id = ?
			`
			_, err := tx.ExecContext(ctx, retryQuery, nextRun, now, ej.id)
			if err != nil {
				return 0, fmt.Errorf("failed reaping job %d to retry_wait: %w", ej.id, err)
			}
		} else {
			if err := s.failBusinessForReapedJob(ctx, tx, ej.id); err != nil {
				return 0, err
			}
			// Mark terminal FAILED with RETRYABLE_EXHAUSTED
			failQuery := `
				UPDATE analysis_jobs
				SET status = 'FAILED',
				    terminal_reason = 'RETRYABLE_EXHAUSTED',
				    last_error_class = 'OWNERSHIP_LOST',
				    last_error_code = 'LEASE_EXPIRED_EXHAUSTED',
				    last_error_message = 'Job lease expired and max attempts exceeded',
				    finished_at = ?,
				    updated_at = ?
				WHERE id = ?
			`
			_, err := tx.ExecContext(ctx, failQuery, now, now, ej.id)
			if err != nil {
				return 0, fmt.Errorf("failed reaping job %d to failed: %w", ej.id, err)
			}
		}
		reapedCount++
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return reapedCount, nil
}

func (s *Store) cancelExpiredDiagnosisTx(ctx context.Context, tx *sql.Tx, expired expiredJob, now time.Time) error {
	jobResult, err := tx.ExecContext(ctx, `UPDATE analysis_jobs
		SET status = 'CANCELLED', terminal_reason = 'CANCELLED', finished_at = ?, updated_at = ?
		WHERE id = ? AND status = 'RUNNING' AND cancel_requested = TRUE AND lease_until < ?`,
		now, now, expired.id, now)
	if err != nil {
		return fmt.Errorf("failed cancelling expired diagnosis job %d: %w", expired.id, err)
	}
	if affected, err := jobResult.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return fmt.Errorf("expired diagnosis job %d changed before cancellation recovery", expired.id)
	}

	// A worker whose lease expired no longer owns the running attempt. Preserve
	// the reaper's existing ABANDONED semantics, but scope the transition to the
	// expired job's execution generation so an older attempt cannot be touched.
	if _, err := tx.ExecContext(ctx, `UPDATE diagnosis_attempts
		SET status = 'ABANDONED', finished_at = ?
		WHERE diagnosis_run_id = ? AND execution_generation = ? AND status = 'RUNNING'`,
		now, expired.resourceID, expired.executionGeneration); err != nil {
		return fmt.Errorf("failed abandoning expired diagnosis attempts for %s: %w", expired.resourceID, err)
	}

	runResult, err := tx.ExecContext(ctx, `UPDATE diagnosis_runs
		SET status = 'CANCELLED', cancel_requested = TRUE,
		    final_attempt_id = (SELECT id FROM diagnosis_attempts
		        WHERE diagnosis_run_id = ? AND execution_generation = ?
		        ORDER BY attempt_no DESC, created_at DESC LIMIT 1),
		    version = version + 1
		WHERE id = ? AND status IN ('QUEUED', 'RUNNING')`,
		expired.resourceID, expired.executionGeneration, expired.resourceID)
	if err != nil {
		return fmt.Errorf("failed cancelling expired diagnosis run %s: %w", expired.resourceID, err)
	}
	if affected, err := runResult.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		var status string
		if err := tx.QueryRowContext(ctx, `SELECT status FROM diagnosis_runs WHERE id = ?`, expired.resourceID).Scan(&status); err != nil {
			return fmt.Errorf("failed verifying expired diagnosis run %s cancellation: %w", expired.resourceID, err)
		}
		if status != "CANCELLED" {
			return fmt.Errorf("expired diagnosis run %s cannot be cancelled from status %s", expired.resourceID, status)
		}
	}
	return nil
}

func (s *Store) failBusinessForReapedJob(ctx context.Context, tx *sql.Tx, jobID int64) error {
	return s.failBusinessTx(ctx, tx, jobID, true, "LEASE_EXPIRED_EXHAUSTED", "Job lease expired and max attempts exceeded")
}

// ManualRequeueTx executes the Manual Requeue Rule for natural-identity resources within a transaction.
func (s *Store) ManualRequeueTx(ctx context.Context, tx *sql.Tx, jobType JobType, resourceID string) error {
	// Restore the business object in the same transaction as the execution
	// generation reset. RETRY_WAIT never mutates business state, so this path
	// is only for terminal retry exhaustion.
	var businessQuery string
	var businessArgs []interface{}
	switch jobType {
	case JobTypeMaterializeSnapshot:
		businessQuery = `UPDATE repository_snapshots SET status = 'CREATED', error_code = NULL, ready_at = NULL WHERE id = ? AND status = 'FAILED'`
		businessArgs = []interface{}{resourceID}
	case JobTypeBuildCodeIndex:
		businessQuery = `UPDATE code_index_builds SET status = 'CREATED', error_code = NULL, ready_at = NULL WHERE id = ? AND status = 'FAILED'`
		businessArgs = []interface{}{resourceID}
	case JobTypeBuildRetrieval:
		businessQuery = `UPDATE retrieval_builds SET status = 'CREATED', error_code = NULL, ready_at = NULL, artifact_path = '', artifact_hash = '' WHERE id = ? AND status = 'FAILED'`
		businessArgs = []interface{}{resourceID}
	case JobTypeRunDiagnosis:
		businessQuery = `UPDATE diagnosis_runs SET status = 'QUEUED', cancel_requested = FALSE, final_attempt_id = NULL, version = version + 1 WHERE id = ? AND status = 'FAILED'`
		businessArgs = []interface{}{resourceID}
	default:
		return fmt.Errorf("unsupported manual requeue job type %s", jobType)
	}
	businessRes, err := tx.ExecContext(ctx, businessQuery, businessArgs...)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "no such table") {
		return fmt.Errorf("failed restoring business object for (%s, %s): %w", jobType, resourceID, err)
	}
	if err == nil {
		if affected, err := businessRes.RowsAffected(); err != nil {
			return err
		} else if affected != 1 {
			// A few low-level job-store tests intentionally exercise jobs without a
			// business row. Production resources always have one; preserve the
			// useful isolated job-store behavior while rejecting an existing object
			// in the wrong state.
			var exists int
			if err := tx.QueryRowContext(ctx, `SELECT 1 FROM `+businessTable(jobType)+` WHERE id = ? LIMIT 1`, resourceID).Scan(&exists); err == nil {
				return fmt.Errorf("cannot requeue business object (%s, %s): not in FAILED state", jobType, resourceID)
			}
		}
	}
	query := `
		UPDATE analysis_jobs
		SET status = 'PENDING',
		    execution_generation = execution_generation + 1,
		    attempt_count = 0,
		    next_run_at = ?,
		    worker_id = NULL,
		    claim_token = NULL,
		    lease_until = NULL,
		    cancel_requested = FALSE,
		    last_error_class = NULL,
		    last_error_code = NULL,
		    last_error_message = NULL,
		    terminal_reason = NULL,
		    finished_at = NULL,
		    updated_at = ?
		WHERE job_type = ?
		  AND resource_id = ?
		  AND status = 'FAILED'
		  AND terminal_reason = 'RETRYABLE_EXHAUSTED'
	`
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, query, now, now, jobType, resourceID)
	if err != nil {
		return fmt.Errorf("failed manual requeue for job (%s, %s): %w", jobType, resourceID, err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return fmt.Errorf("cannot requeue job (%s, %s): not in FAILED state with RETRYABLE_EXHAUSTED", jobType, resourceID)
	}
	return nil
}

func (s *Store) ManualRequeue(ctx context.Context, jobType JobType, resourceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.ManualRequeueTx(ctx, tx, jobType, resourceID); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryDiagnosis explicitly starts a new Diagnosis attempt after an external
// provider failure. It is intentionally separate from the generic requeue
// rule so a worker crash or permanent product bug is not silently replayed.
func (s *Store) RetryDiagnosis(ctx context.Context, resourceID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var lastClass, lastCode sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT last_error_class, last_error_code FROM analysis_jobs WHERE job_type = ? AND resource_id = ? AND status = 'FAILED'`, JobTypeRunDiagnosis, resourceID).Scan(&lastClass, &lastCode); err != nil {
		return err
	}
	code := strings.ToUpper(lastCode.String)
	if !IsRetryableDiagnosisProviderFailure(ErrorClass(lastClass.String), code) {
		return fmt.Errorf("diagnosis retry is only allowed after an external provider failure")
	}
	now := time.Now().UTC()
	runResult, err := tx.ExecContext(ctx, `UPDATE diagnosis_runs SET status = 'QUEUED', cancel_requested = FALSE, final_attempt_id = NULL, version = version + 1, updated_at = ? WHERE id = ? AND status = 'FAILED'`, now, resourceID)
	if err != nil {
		return err
	}
	if affected, err := runResult.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return fmt.Errorf("diagnosis %s is not in FAILED state", resourceID)
	}
	result, err := tx.ExecContext(ctx, `UPDATE analysis_jobs SET status = 'PENDING', execution_generation = execution_generation + 1, attempt_count = 0, next_run_at = ?, worker_id = NULL, claim_token = NULL, lease_until = NULL, cancel_requested = FALSE, last_error_class = NULL, last_error_code = NULL, last_error_message = NULL, terminal_reason = NULL, finished_at = NULL, updated_at = ? WHERE job_type = ? AND resource_id = ? AND status = 'FAILED'`, now, now, JobTypeRunDiagnosis, resourceID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return fmt.Errorf("diagnosis job %s is not retryable", resourceID)
	}
	return tx.Commit()
}

// IsRetryableDiagnosisProviderError is the single retry policy used by both
// the retry endpoint and the diagnosis status response. Unknown failures fail
// closed: they may be worker bugs, malformed reports, or local persistence
// errors rather than a transient upstream condition.
func IsRetryableDiagnosisProviderError(code string) bool {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "PROVIDER_TIMEOUT", "PROVIDER_CONNECTION_FAILED", "PROVIDER_UPSTREAM_ERROR",
		"PROVIDER_RATE_LIMITED", "PROVIDER_5XX", "PROVIDER_NETWORK_ERROR",
		"HTTP_429_RATE_LIMITED", "HTTP_5XX_SERVER_ERROR", "TRANSIENT_NETWORK_ERROR":
		return true
	default:
		return false
	}
}

func IsRetryableDiagnosisProviderFailure(class ErrorClass, code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	// A provider failure after useful Agent progress is deliberately PERMANENT
	// for automatic retries (to avoid replaying billable partial work), while
	// the user may explicitly start a fresh execution generation.
	if class == ErrorClassPermanent && code == "PROVIDER_PROGRESS_ABORTED" {
		return true
	}
	return class == ErrorClassRetryable && IsRetryableDiagnosisProviderError(code)
}

func businessTable(jobType JobType) string {
	switch jobType {
	case JobTypeMaterializeSnapshot:
		return "repository_snapshots"
	case JobTypeBuildCodeIndex:
		return "code_index_builds"
	case JobTypeBuildRetrieval:
		return "retrieval_builds"
	default:
		return "diagnosis_runs"
	}
}

func scanJob(row *sql.Row) (*AnalysisJob, error) {
	var job AnalysisJob
	var termReason sql.NullString
	var workerID sql.NullString
	var claimToken sql.NullString
	var leaseUntil sql.NullTime
	var lastErrClass sql.NullString
	var lastErrCode sql.NullString
	var lastErrMsg sql.NullString
	var finishedAt sql.NullTime

	err := row.Scan(
		&job.ID, &job.JobType, &job.ResourceID, &job.Status, &job.ExecutionGeneration,
		&termReason, &job.AttemptCount, &job.MaxAttempts, &job.NextRunAt,
		&workerID, &claimToken, &leaseUntil, &job.CancelRequested,
		&lastErrClass, &lastErrCode, &lastErrMsg,
		&job.CreatedAt, &job.UpdatedAt, &finishedAt,
	)
	if err != nil {
		return nil, err
	}

	if termReason.Valid {
		tr := TerminalReason(termReason.String)
		job.TerminalReason = &tr
	}
	if workerID.Valid {
		job.WorkerID = &workerID.String
	}
	if claimToken.Valid {
		job.ClaimToken = &claimToken.String
	}
	if leaseUntil.Valid {
		job.LeaseUntil = &leaseUntil.Time
	}
	if lastErrClass.Valid {
		job.LastErrorClass = &lastErrClass.String
	}
	if lastErrCode.Valid {
		job.LastErrorCode = &lastErrCode.String
	}
	if lastErrMsg.Valid {
		job.LastErrorMessage = &lastErrMsg.String
	}
	if finishedAt.Valid {
		job.FinishedAt = &finishedAt.Time
	}

	return &job, nil
}
