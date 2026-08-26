package retrieval

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
	"repolens/internal/retrieval/structural"
)

// ProductionRetriever implements Retriever using the pure Go BM25 and Structural Code Intelligence engine.
type ProductionRetriever struct {
	mu             sync.RWMutex
	ciStore        codeintelstore.Store
	baseStorageDir string
	indexCache     map[int64]*bm25.Index
}

// NewProductionRetriever constructs the production retrieval adapter.
func NewProductionRetriever(ciStore codeintelstore.Store, baseStorageDir string) *ProductionRetriever {
	return &ProductionRetriever{
		ciStore:        ciStore,
		baseStorageDir: baseStorageDir,
		indexCache:     make(map[int64]*bm25.Index),
	}
}

// Search queries the authoritative pinned retrieval index for the requested snapshot.
func (r *ProductionRetriever) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if req.SnapshotID == "" {
		return nil, fmt.Errorf("snapshot_id is required for retrieval")
	}

	// Production requests must carry the identities captured by Diagnosis.
	var cib *codeintelmodel.CodeIndexBuild
	var rb *codeintelmodel.RetrievalBuild
	if req.CodeIndexBuildID <= 0 || req.RetrievalBuildID <= 0 {
		return nil, fmt.Errorf("both code_index_build_id and retrieval_build_id are required")
	}
	var err error
	cib, err = r.ciStore.GetByID(ctx, req.CodeIndexBuildID)
	if err != nil {
		return nil, fmt.Errorf("pinned code index build not found: %w", err)
	}
	rb, err = r.ciStore.GetRetrievalBuildByID(ctx, req.RetrievalBuildID)
	if err != nil {
		return nil, fmt.Errorf("pinned retrieval build not found: %w", err)
	}
	if cib.Status != codeintelmodel.BuildStatusReady || rb.Status != codeintelmodel.BuildStatusReady || rb.CodeIndexBuildID != cib.ID {
		return nil, fmt.Errorf("pinned retrieval lineage is not READY or does not match")
	}
	if cib.SnapshotID != req.SnapshotID {
		return nil, fmt.Errorf("pinned code index build belongs to snapshot %s, not %s", cib.SnapshotID, req.SnapshotID)
	}

	// 2. Load BM25 Index (with in-memory caching)
	r.mu.RLock()
	idx, exists := r.indexCache[rb.ID]
	r.mu.RUnlock()

	if !exists {
		artifactPath := rb.ArtifactPath
		if artifactPath == "" {
			artifactPath = filepath.Join(r.baseStorageDir, fmt.Sprintf("%d", rb.ID))
		}
		loaded, loadErr := artifact.LoadIndexVerified(artifactPath, rb.ID, rb.ArtifactHash)
		if loadErr != nil {
			return nil, fmt.Errorf("failed loading retrieval artifact from %s: %w", artifactPath, loadErr)
		}
		r.mu.Lock()
		r.indexCache[rb.ID] = loaded
		idx = loaded
		r.mu.Unlock()
	}

	// 3. Execute Structural Retrieval
	topK := req.TopK
	if topK <= 0 {
		topK = 20
	}

	engine := structural.NewEngine(idx, r.ciStore, cib.ID)
	structResults := engine.Search(ctx, req.Query, topK)

	// 4. Map to generic SearchResult
	var searchResults []SearchResult
	for _, sr := range structResults {
		searchResults = append(searchResults, SearchResult{
			ChunkID:         fmt.Sprintf("%s:%d-%d", sr.Document.FilePath, sr.Document.StartLine, sr.Document.EndLine),
			Path:            sr.Document.FilePath,
			Language:        "go",
			Symbol:          sr.Document.SymbolName,
			StartLine:       sr.Document.StartLine,
			EndLine:         sr.Document.EndLine,
			Snippet:         sr.Document.Content,
			Score:           sr.FinalScore,
			RetrievalSource: "symbol_bm25_structural",
		})
	}

	return searchResults, nil
}
