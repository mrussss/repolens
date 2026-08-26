package jobs

import (
	"errors"
	"time"
)

// ErrOwnershipLost is returned when a worker attempts to renew a lease or finalize a job after its claim has expired or been stolen.
var ErrOwnershipLost = errors.New("job ownership lost: claim token or lease is no longer valid")

// JobType represents the type of async job.
type JobType string

const (
	JobTypeMaterializeSnapshot JobType = "MATERIALIZE_SNAPSHOT"
	JobTypeBuildCodeIndex      JobType = "BUILD_CODE_INDEX"
	JobTypeBuildRetrieval      JobType = "BUILD_RETRIEVAL"
	JobTypeRunDiagnosis        JobType = "RUN_DIAGNOSIS"
)

// JobStatus represents the execution state of an AnalysisJob.
type JobStatus string

const (
	StatusPending   JobStatus = "PENDING"
	StatusRunning   JobStatus = "RUNNING"
	StatusRetryWait JobStatus = "RETRY_WAIT"
	StatusSucceeded JobStatus = "SUCCEEDED"
	StatusFailed    JobStatus = "FAILED"
	StatusCancelled JobStatus = "CANCELLED"
)

// TerminalReason represents why a job reached a terminal state.
type TerminalReason string

const (
	TerminalReasonRetryableExhausted TerminalReason = "RETRYABLE_EXHAUSTED"
	TerminalReasonPermanent          TerminalReason = "PERMANENT"
	TerminalReasonCancelled          TerminalReason = "CANCELLED"
)

// ErrorClass categorizes failure behavior.
type ErrorClass string

const (
	ErrorClassRetryable     ErrorClass = "RETRYABLE"
	ErrorClassPermanent     ErrorClass = "PERMANENT"
	ErrorClassCancelled     ErrorClass = "CANCELLED"
	ErrorClassOwnershipLost ErrorClass = "OWNERSHIP_LOST"
)

// AnalysisJob is the persistent asynchronous job model.
type AnalysisJob struct {
	ID                  int64           `json:"id" gorm:"primaryKey;autoIncrement"`
	JobType             JobType         `json:"job_type" gorm:"size:64;not null;uniqueIndex:uq_job_resource,priority:1"`
	ResourceID          string          `json:"resource_id" gorm:"size:64;not null;uniqueIndex:uq_job_resource,priority:2"`
	Status              JobStatus       `json:"status" gorm:"size:32;not null;default:'PENDING';index:ix_job_claim,priority:1;index:ix_job_lease,priority:1"`
	ExecutionGeneration int             `json:"execution_generation" gorm:"not null;default:1"`
	TerminalReason      *TerminalReason `json:"terminal_reason,omitempty" gorm:"size:32"`
	AttemptCount        int             `json:"attempt_count" gorm:"not null;default:0"`
	MaxAttempts         int             `json:"max_attempts" gorm:"not null;default:3"`
	NextRunAt           time.Time       `json:"next_run_at" gorm:"not null;index:ix_job_claim,priority:2"`
	WorkerID            *string         `json:"worker_id,omitempty" gorm:"size:64"`
	ClaimToken          *string         `json:"claim_token,omitempty" gorm:"size:64"`
	LeaseUntil          *time.Time      `json:"lease_until,omitempty" gorm:"index:ix_job_lease,priority:2"`
	CancelRequested     bool            `json:"cancel_requested" gorm:"not null;default:false"`
	LastErrorClass      *string         `json:"last_error_class,omitempty" gorm:"size:32"`
	LastErrorCode       *string         `json:"last_error_code,omitempty" gorm:"size:64"`
	LastErrorMessage    *string         `json:"last_error_message,omitempty" gorm:"type:text"`
	CreatedAt           time.Time       `json:"created_at" gorm:"index:ix_job_claim,priority:3"`
	UpdatedAt           time.Time       `json:"updated_at"`
	FinishedAt          *time.Time      `json:"finished_at,omitempty"`
}

func (AnalysisJob) TableName() string {
	return "analysis_jobs"
}
