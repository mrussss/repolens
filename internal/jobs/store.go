package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Store defines database operations on analysis_jobs.
type Store struct {
	db     *sql.DB
	driver string
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
		ORDER BY created_at, id
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
	`
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, query, newLeaseUntil, now, jobID, workerID, claimToken)
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
	now := time.Now().UTC()
	var query string
	var err error
	var res sql.Result

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
		res, err = tx.ExecContext(ctx, query, terminalReason, string(errClass), errCode, errMsg, now, now, jobID, workerID, claimToken)
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
		res, err = tx.ExecContext(ctx, query, nextRunAt, string(errClass), errCode, errMsg, now, jobID, workerID, claimToken)
	}

	if err != nil {
		return fmt.Errorf("failed finalizing failure for job %d: %w", jobID, err)
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

// ReapExpiredJobs scans for RUNNING jobs with expired leases and moves them to RETRY_WAIT or terminal FAILED.
func (s *Store) ReapExpiredJobs(ctx context.Context, batchSize int) (int, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	selectQuery := `
		SELECT id, attempt_count, max_attempts
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

	type expiredJob struct {
		id           int64
		attemptCount int
		maxAttempts  int
	}
	var expired []expiredJob
	for rows.Next() {
		var ej expiredJob
		if err := rows.Scan(&ej.id, &ej.attemptCount, &ej.maxAttempts); err != nil {
			return 0, err
		}
		expired = append(expired, ej)
	}
	rows.Close()

	reapedCount := 0
	for _, ej := range expired {
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

// ManualRequeueTx executes the Manual Requeue Rule for natural-identity resources within a transaction.
func (s *Store) ManualRequeueTx(ctx context.Context, tx *sql.Tx, jobType JobType, resourceID string) error {
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
