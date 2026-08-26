package retrieval

import (
	"context"
)

type SearchRequest struct {
	SnapshotID       string `json:"snapshot_id"`
	CodeIndexBuildID int64  `json:"code_index_build_id,omitempty"`
	RetrievalBuildID int64  `json:"retrieval_build_id,omitempty"`
	Query            string `json:"query"`
	TopK             int    `json:"top_k"`
	Scope            string `json:"scope,omitempty"` // e.g. file path prefix or extension
}

type SearchResult struct {
	ChunkID         string  `json:"chunk_id"`
	Path            string  `json:"path"`
	Language        string  `json:"language"`
	Symbol          string  `json:"symbol,omitempty"`
	StartLine       int     `json:"start_line"`
	EndLine         int     `json:"end_line"`
	Snippet         string  `json:"snippet"`
	Score           float64 `json:"score"`
	RetrievalSource string  `json:"retrieval_source"` // "lexical", "bm25", "vector", "hybrid_rrf"
}

type Retriever interface {
	Search(ctx context.Context, req SearchRequest) ([]SearchResult, error)
}
