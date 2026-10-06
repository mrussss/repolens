package retrieval

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
	"repolens/internal/retrieval/structural"
)

// ErrUnsupportedStrategy identifies an unsupported pinned build strategy.
var ErrUnsupportedStrategy = errors.New("unsupported retrieval strategy")

type indexCacheKey struct {
	BuildID              int64
	Path, Hash, Strategy string
}

// ProductionRetriever executes the strategy pinned by the retrieval build.
type ProductionRetriever struct {
	mu             sync.RWMutex
	ciStore        codeintelstore.Store
	baseStorageDir string
	indexCache     map[indexCacheKey]*bm25.Index
}

// NewProductionRetriever constructs the production retrieval adapter.
func NewProductionRetriever(ciStore codeintelstore.Store, baseStorageDir string) *ProductionRetriever {
	return &ProductionRetriever{
		ciStore:        ciStore,
		baseStorageDir: baseStorageDir,
		indexCache:     make(map[indexCacheKey]*bm25.Index),
	}
}

// Search queries the authoritative pinned retrieval index for the requested snapshot.
func (r *ProductionRetriever) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	results, _, err := r.SearchWithTrace(ctx, req)
	return results, err
}

// SearchWithTrace executes the same pinned production path, exposing V2's
// request-local diagnostic work and candidates without a separate eval engine.
func (r *ProductionRetriever) SearchWithTrace(ctx context.Context, req SearchRequest) ([]SearchResult, *structural.ExpansionSearchTrace, error) {
	if req.SnapshotID == "" {
		return nil, nil, fmt.Errorf("snapshot_id is required for retrieval")
	}

	// Production requests must carry the identities captured by Diagnosis.
	var cib *codeintelmodel.CodeIndexBuild
	var rb *codeintelmodel.RetrievalBuild
	if req.CodeIndexBuildID <= 0 || req.RetrievalBuildID <= 0 {
		return nil, nil, fmt.Errorf("both code_index_build_id and retrieval_build_id are required")
	}
	var err error
	cib, err = r.ciStore.GetByID(ctx, req.CodeIndexBuildID)
	if err != nil {
		return nil, nil, fmt.Errorf("pinned code index build not found: %w", err)
	}
	rb, err = r.ciStore.GetRetrievalBuildByID(ctx, req.RetrievalBuildID)
	if err != nil {
		return nil, nil, fmt.Errorf("pinned retrieval build not found: %w", err)
	}
	if cib.Status != codeintelmodel.BuildStatusReady || rb.Status != codeintelmodel.BuildStatusReady || rb.CodeIndexBuildID != cib.ID {
		return nil, nil, fmt.Errorf("pinned retrieval lineage is not READY or does not match")
	}
	if cib.SnapshotID != req.SnapshotID {
		return nil, nil, fmt.Errorf("pinned code index build belongs to snapshot %s, not %s", cib.SnapshotID, req.SnapshotID)
	}

	if rb.Strategy != codeintelmodel.StrategyBM25 && rb.Strategy != codeintelmodel.StrategyBM25Structural && rb.Strategy != codeintelmodel.StrategyBM25StructuralV2 {
		return nil, nil, fmt.Errorf("%w: %s", ErrUnsupportedStrategy, rb.Strategy)
	}
	cacheKey := indexCacheKey{rb.ID, rb.ArtifactPath, rb.ArtifactHash, rb.Strategy}

	// 2. Load BM25 Index (with in-memory caching)
	r.mu.RLock()
	idx, exists := r.indexCache[cacheKey]
	r.mu.RUnlock()

	if !exists {
		artifactPath := rb.ArtifactPath
		if artifactPath == "" {
			artifactPath = filepath.Join(r.baseStorageDir, fmt.Sprintf("%d", rb.ID))
		}
		loaded, loadErr := artifact.LoadIndexVerified(artifactPath, rb.ID, rb.ArtifactHash, rb.Strategy)
		if loadErr != nil {
			return nil, nil, fmt.Errorf("failed loading retrieval artifact from %s: %w", artifactPath, loadErr)
		}
		r.mu.Lock()
		r.indexCache[cacheKey] = loaded
		idx = loaded
		r.mu.Unlock()
	}

	// Execute exactly the pinned strategy with the requested result budget.
	topK := req.TopK
	if topK <= 0 {
		topK = 20
	}

	if rb.Strategy == codeintelmodel.StrategyBM25 {
		var results []SearchResult
		for _, hit := range idx.Search(req.Query, topK) {
			results = append(results, mapSearchResult(hit.Document, hit.Score, "symbol_bm25", "BM25", req.Query))
		}
		return results, nil, nil
	}

	if rb.Strategy == codeintelmodel.StrategyBM25StructuralV2 {
		hits, trace, err := structural.NewExpansionEngine(idx, r.ciStore, cib.ID).Search(ctx, req.Query, topK)
		if err != nil {
			return nil, trace, err
		}
		var results []SearchResult
		for _, hit := range hits {
			reason := "BM25"
			if hit.Trace.Expanded {
				reason = hit.Trace.BestSeed.Reason
			}
			// Score remains lexical evidence, not the V2 rank-placement key.
			// An added document has no lexical score; typed trace explains rank.
			mapped := mapSearchResult(hit.Document, hit.BaseScore, "symbol_bm25_structural_v2", reason, req.Query)
			detail := hit.Trace
			mapped.StructuralV2 = &detail
			results = append(results, mapped)
		}
		return results, trace, nil
	}

	engine := structural.NewEngine(idx, r.ciStore, cib.ID)
	structResults := engine.Search(ctx, req.Query, topK)

	// 4. Map to generic SearchResult
	var searchResults []SearchResult
	for _, sr := range structResults {
		reason := strings.Join(sr.StructuralReasons, ",")
		if reason == "" {
			reason = "BM25"
		}
		searchResults = append(searchResults, mapSearchResult(sr.Document, sr.FinalScore, "symbol_bm25_structural", reason, req.Query))
	}

	return searchResults, nil, nil
}

func mapSearchResult(doc bm25.Document, score float64, source, reason, query string) SearchResult {
	var keys []string
	if doc.SymbolKeyHash != "" {
		keys = []string{doc.SymbolKeyHash}
	}
	return SearchResult{
		ChunkID: fmt.Sprintf("%s:%d-%d", doc.FilePath, doc.StartLine, doc.EndLine),
		Path:    doc.FilePath, Language: "go", Symbol: doc.SymbolName,
		StartLine: doc.StartLine, EndLine: doc.EndLine, Snippet: doc.Content,
		Score: score, RetrievalSource: source, RetrievalReason: reason, SymbolKeys: keys,
		MatchedTerms: matchedTerms(query, doc.Content+" "+doc.SymbolName+" "+doc.FilePath),
	}
}
