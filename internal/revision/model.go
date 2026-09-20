package revision

import "time"

type Status string

const (
	StatusPreparing Status = "PREPARING"
	StatusReady     Status = "READY"
	StatusFailed    Status = "FAILED"
)

type Stage string

const (
	StageMaterializing  Stage = "MATERIALIZING"
	StageBuildingCode   Stage = "BUILDING_CODE_INDEX"
	StageBuildingSearch Stage = "BUILDING_RETRIEVAL"
	StageReady          Stage = "READY"
	StageFailed         Stage = "FAILED"
)

// AnalysisRevision is the product-facing immutable analysis lineage. The
// build IDs are implementation details kept here so a diagnosis can be
// reproduced without asking the client to understand the build pipeline.
type AnalysisRevision struct {
	ID                  string     `gorm:"primaryKey;size:36" json:"id"`
	RepositoryID        string     `gorm:"size:36;not null;index:idx_revision_repository;uniqueIndex:uq_revision_identity,priority:1" json:"repository_id"`
	SourceRef           string     `gorm:"size:255;not null" json:"source_ref"`
	CommitSHA           string     `gorm:"size:40;not null;uniqueIndex:uq_revision_identity,priority:2" json:"commit_sha"`
	PipelineVersion     string     `gorm:"size:64;not null" json:"pipeline_version"`
	PipelineFingerprint string     `gorm:"size:64;not null;uniqueIndex:uq_revision_identity,priority:3" json:"pipeline_fingerprint"`
	SnapshotID          string     `gorm:"size:64;index" json:"snapshot_id,omitempty"`
	CodeIndexBuildID    int64      `gorm:"index" json:"code_index_build_id,omitempty"`
	RetrievalBuildID    int64      `gorm:"index" json:"retrieval_build_id,omitempty"`
	Status              Status     `gorm:"size:32;not null;index" json:"status"`
	Stage               Stage      `gorm:"size:64;not null" json:"stage"`
	ErrorCode           string     `gorm:"size:64" json:"error_code,omitempty"`
	ErrorMessage        string     `gorm:"type:text" json:"error_message,omitempty"`
	ExecutionGeneration int        `gorm:"not null;default:1" json:"execution_generation"`
	Version             int        `gorm:"not null;default:1" json:"version"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	ReadyAt             *time.Time `json:"ready_at,omitempty"`
}

func (AnalysisRevision) TableName() string { return "analysis_revisions" }

func IsValidTransition(from Status, to Status) bool {
	switch from {
	case StatusPreparing:
		return to == StatusReady || to == StatusFailed
	case StatusFailed:
		return to == StatusPreparing
	default:
		return false
	}
}
