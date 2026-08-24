package diagnosis

import (
	"time"
)

type RunStatus string

const (
	StatusQueued          RunStatus = "QUEUED"
	StatusRunning         RunStatus = "RUNNING"
	StatusSucceeded       RunStatus = "SUCCEEDED"
	StatusRetryWait       RunStatus = "RETRY_WAIT"
	StatusFailed          RunStatus = "FAILED"
	StatusCancelRequested RunStatus = "CANCEL_REQUESTED"
	StatusCancelled       RunStatus = "CANCELLED"
)

type DiagnosisRun struct {
	ID                     string    `gorm:"primaryKey;size:36" json:"id"`
	UserID                 string    `gorm:"size:36;not null;index:idx_user_idemp,unique;index:idx_user_created" json:"user_id"`
	RepositoryID           string    `gorm:"size:36;not null;index" json:"repository_id"`
	SnapshotID             string    `gorm:"size:36;not null;index" json:"snapshot_id"`
	IssueTitle             string    `gorm:"size:255;not null" json:"issue_title"`
	IssueDescription       string    `gorm:"type:text" json:"issue_description"`
	ErrorLog               string    `gorm:"type:mediumtext" json:"error_log"`
	Status                 RunStatus `gorm:"size:32;not null;default:'QUEUED';index" json:"status"`
	CancelRequested        bool      `gorm:"default:false;not null" json:"cancel_requested"`
	IdempotencyKey         string    `gorm:"size:128;not null;index:idx_user_idemp,unique" json:"idempotency_key"`
	IdempotencyRequestHash string    `gorm:"size:64;not null" json:"idempotency_request_hash"`
	FinalAttemptID         string    `gorm:"size:36" json:"final_attempt_id,omitempty"`
	Version                int       `gorm:"default:1;not null" json:"version"`
	CreatedAt              time.Time `json:"created_at"`
	UpdatedAt              time.Time `json:"updated_at"`
}
