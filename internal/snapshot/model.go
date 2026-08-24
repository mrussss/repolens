package snapshot

import (
	"time"
)

type SnapshotStatus string

const (
	StatusCreated           SnapshotStatus = "CREATED"
	StatusMaterializing     SnapshotStatus = "MATERIALIZING"
	StatusReady             SnapshotStatus = "READY"
	StatusMaterializeFailed SnapshotStatus = "MATERIALIZE_FAILED"
)

type RepositorySnapshot struct {
	ID               string         `gorm:"primaryKey;size:36" json:"id"`
	RepositoryID     string         `gorm:"index;size:36;not null" json:"repository_id"`
	CommitSHA        string         `gorm:"size:64;not null" json:"commit_sha"`
	Ref              string         `gorm:"size:128;not null" json:"ref"`
	MaterializedPath string         `gorm:"size:512;not null" json:"materialized_path"`
	ContentHash      string         `gorm:"size:64;not null" json:"content_hash"`
	Status           SnapshotStatus `gorm:"size:32;not null;default:'CREATED'" json:"status"`
	CreatedAt        time.Time      `json:"created_at"`
	ReadyAt          *time.Time     `json:"ready_at,omitempty"`
}
