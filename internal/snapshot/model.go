package snapshot

import (
	"time"
)

type SnapshotStatus string

const (
	StatusCreated       SnapshotStatus = "CREATED"
	StatusMaterializing SnapshotStatus = "MATERIALIZING"
	StatusReady         SnapshotStatus = "READY"
	StatusFailed        SnapshotStatus = "FAILED"
)

type RepositorySnapshot struct {
	ID               string         `gorm:"primaryKey;size:64" json:"id"`
	RepositoryID     string         `gorm:"size:64;not null;uniqueIndex:uq_repo_commit,priority:1;index:ix_repo_snap" json:"repository_id"`
	CommitSHA        string         `gorm:"size:64;not null;uniqueIndex:uq_repo_commit,priority:2" json:"commit_sha"`
	Ref              string         `gorm:"size:128;not null" json:"ref"`
	RequestedRef     string         `gorm:"size:128" json:"requested_ref"`
	MaterializedPath string         `gorm:"size:512;not null" json:"materialized_path"`
	ContentHash      string         `gorm:"size:64;not null" json:"content_hash"`
	Status           SnapshotStatus `gorm:"size:32;not null;default:'CREATED'" json:"status"`
	FileCount        int            `gorm:"not null;default:0" json:"file_count"`
	TotalBytes       int64          `gorm:"not null;default:0" json:"total_bytes"`
	ErrorCode        string         `gorm:"size:64" json:"error_code,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
	ReadyAt          *time.Time     `json:"ready_at,omitempty"`
}

func (RepositorySnapshot) TableName() string {
	return "repository_snapshots"
}
