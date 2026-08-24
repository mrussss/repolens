package repoindex

import (
	"time"
)

type IndexStatus string

const (
	StatusCreated     IndexStatus = "CREATED"
	StatusIndexQueued IndexStatus = "INDEX_QUEUED"
	StatusIndexing    IndexStatus = "INDEXING"
	StatusReady       IndexStatus = "READY"
	StatusIndexFailed IndexStatus = "INDEX_FAILED"
)

type RetrievalStrategy string

const (
	StrategyLexical RetrievalStrategy = "LEXICAL"
	StrategyBM25    RetrievalStrategy = "BM25"
	StrategyVector  RetrievalStrategy = "VECTOR"
	StrategyHybrid  RetrievalStrategy = "HYBRID"
)

type RepositoryIndex struct {
	ID               string            `gorm:"primaryKey;size:36" json:"id"`
	SnapshotID       string            `gorm:"index;size:36;not null" json:"snapshot_id"`
	Strategy         RetrievalStrategy `gorm:"size:32;not null" json:"strategy"`
	IndexVersion     string            `gorm:"size:32;not null;default:'v1'" json:"index_version"`
	Status           IndexStatus       `gorm:"size:32;not null;default:'CREATED'" json:"status"`
	ChunkCount       int               `gorm:"default:0" json:"chunk_count"`
	DocumentCount    int               `gorm:"default:0" json:"document_count"`
	EmbeddingModel   string            `gorm:"size:64" json:"embedding_model,omitempty"`
	EmbeddingVersion string            `gorm:"size:32" json:"embedding_version,omitempty"`
	ErrorCode        string            `gorm:"size:64" json:"error_code,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	ReadyAt          *time.Time        `json:"ready_at,omitempty"`
}
