package model

import (
	"time"
)

// BuildStatus represents the lifecycle state of a build artifact (CodeIndexBuild or RetrievalBuild).
type BuildStatus string

const (
	BuildStatusCreated  BuildStatus = "CREATED"
	BuildStatusBuilding BuildStatus = "BUILDING"
	BuildStatusReady    BuildStatus = "READY"
	BuildStatusFailed   BuildStatus = "FAILED"
)

// Version constants frozen for v2.1
const (
	CurrentParserVersion       = "v2.1.0"
	CurrentAnalyzerVersion     = "v2.1.0"
	CurrentSymbolSchemaVersion = "v2.1.0"
	CurrentRetrievalVersion    = "v2.1.0"
	CurrentTokenizerVersion    = "v2.1.0"
)

// CodeIndexBuild represents the authoritative versioned structural code intelligence build for a snapshot.
type CodeIndexBuild struct {
	ID                  int64       `json:"id" gorm:"primaryKey;autoIncrement"`
	SnapshotID          string      `json:"snapshot_id" gorm:"size:64;not null;uniqueIndex:uq_code_index_build,priority:1;index:ix_cib_snap"`
	ParserVersion       string      `json:"parser_version" gorm:"size:32;not null;uniqueIndex:uq_code_index_build,priority:2"`
	AnalyzerVersion     string      `json:"analyzer_version" gorm:"size:32;not null;uniqueIndex:uq_code_index_build,priority:3"`
	SymbolSchemaVersion string      `json:"symbol_schema_version" gorm:"size:32;not null;uniqueIndex:uq_code_index_build,priority:4"`
	BuildContextHash    string      `json:"build_context_hash" gorm:"size:64;not null;uniqueIndex:uq_code_index_build,priority:5"`
	ModulePath          string      `json:"module_path" gorm:"size:255;not null"`
	GOOS                string      `json:"goos" gorm:"size:32;not null;default:'linux'"`
	GOARCH              string      `json:"goarch" gorm:"size:32;not null;default:'amd64'"`
	BuildTagsHash       string      `json:"build_tags_hash" gorm:"size:64;not null"`
	Status              BuildStatus `json:"status" gorm:"size:32;not null;default:'CREATED';index:ix_cib_status"`

	// Completeness & Quality metrics
	FilesTotal              int `json:"files_total" gorm:"not null;default:0"`
	FilesParsed             int `json:"files_parsed" gorm:"not null;default:0"`
	FilesFailed             int `json:"files_failed" gorm:"not null;default:0"`
	PackagesTotal           int `json:"packages_total" gorm:"not null;default:0"`
	PackagesTypechecked     int `json:"packages_typechecked" gorm:"not null;default:0"`
	PackagesFailed          int `json:"packages_failed" gorm:"not null;default:0"`
	SymbolCount             int `json:"symbol_count" gorm:"not null;default:0"`
	SemanticRelationCount   int `json:"semantic_relation_count" gorm:"not null;default:0"`
	SyntacticRelationCount  int `json:"syntactic_relation_count" gorm:"not null;default:0"`
	HeuristicRelationCount  int `json:"heuristic_relation_count" gorm:"not null;default:0"`
	UnresolvedRelationCount int `json:"unresolved_relation_count" gorm:"not null;default:0"`

	ErrorCode string     `json:"error_code,omitempty" gorm:"size:64"`
	CreatedAt time.Time  `json:"created_at"`
	ReadyAt   *time.Time `json:"ready_at,omitempty"`
}

func (CodeIndexBuild) TableName() string {
	return "code_index_builds"
}

// RetrievalBuild represents a versioned lexical or BM25 retrieval index build derived from a CodeIndexBuild.
type RetrievalBuild struct {
	ID               int64       `json:"id" gorm:"primaryKey;autoIncrement"`
	CodeIndexBuildID int64       `json:"code_index_build_id" gorm:"not null;uniqueIndex:uq_retrieval_build,priority:1;index:ix_rb_cib"`
	Strategy         string      `json:"strategy" gorm:"size:32;not null;uniqueIndex:uq_retrieval_build,priority:2"` // e.g. "BM25"
	RetrievalVersion string      `json:"retrieval_version" gorm:"size:32;not null;uniqueIndex:uq_retrieval_build,priority:3"`
	TokenizerVersion string      `json:"tokenizer_version" gorm:"size:32;not null;uniqueIndex:uq_retrieval_build,priority:4"`
	ConfigHash       string      `json:"config_hash" gorm:"size:64;not null;uniqueIndex:uq_retrieval_build,priority:5"`
	ArtifactPath     string      `json:"artifact_path" gorm:"size:512;not null"`
	ArtifactHash     string      `json:"artifact_hash" gorm:"size:64;not null"`
	DocumentCount    int         `json:"document_count" gorm:"not null;default:0"`
	Status           BuildStatus `json:"status" gorm:"size:32;not null;default:'CREATED';index:ix_rb_status"`
	ErrorCode        string      `json:"error_code,omitempty" gorm:"size:64"`
	CreatedAt        time.Time   `json:"created_at"`
	ReadyAt          *time.Time  `json:"ready_at,omitempty"`
}

func (RetrievalBuild) TableName() string {
	return "retrieval_builds"
}
