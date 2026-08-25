package retrieval

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"repolens/internal/indexing"
)

type VectorRetriever struct {
	chunkStore ChunkIndexStore
	embedder   EmbeddingProvider
	cacheMu    sync.RWMutex
	cache      map[string][]float32 // chunkID -> embedding vector
}

func NewVectorRetriever(chunkStore ChunkIndexStore, embedder ...EmbeddingProvider) *VectorRetriever {
	var emb EmbeddingProvider
	if len(embedder) > 0 && embedder[0] != nil {
		emb = embedder[0]
	} else {
		emb = NewLocalTFIDFEmbeddingProvider(128)
	}
	return &VectorRetriever{
		chunkStore: chunkStore,
		embedder:   emb,
		cache:      make(map[string][]float32),
	}
}

func (r *VectorRetriever) EmbeddingModel() string {
	return r.embedder.Model()
}

func (r *VectorRetriever) EmbeddingDimension() int {
	return r.embedder.Dimension()
}

func CosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	dot := 0.0
	for i := range a {
		dot += float64(a[i] * b[i])
	}
	return dot
}

func (r *VectorRetriever) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	chunks := r.chunkStore.GetChunks(req.SnapshotID)
	if len(chunks) == 0 {
		return nil, nil
	}

	if req.TopK <= 0 {
		req.TopK = 10
	}

	// 1. Embed query
	qVecs, err := r.embedder.Embed(ctx, []string{req.Query})
	if err != nil {
		return nil, fmt.Errorf("failed to embed search query: %w", err)
	}
	if len(qVecs) == 0 || len(qVecs[0]) == 0 {
		return nil, nil
	}
	qVec := qVecs[0]

	// 2. Embed missing chunks in batches
	var uncachedChunks []indexing.CodeChunk
	var uncachedTexts []string

	r.cacheMu.RLock()
	for _, c := range chunks {
		if _, exists := r.cache[c.ID]; !exists {
			uncachedChunks = append(uncachedChunks, c)
			uncachedTexts = append(uncachedTexts, c.Content+" "+c.Symbol+" "+c.Path)
		}
	}
	r.cacheMu.RUnlock()

	if len(uncachedTexts) > 0 {
		cVecs, err := r.embedder.Embed(ctx, uncachedTexts)
		if err == nil && len(cVecs) == len(uncachedChunks) {
			r.cacheMu.Lock()
			for i, c := range uncachedChunks {
				r.cache[c.ID] = cVecs[i]
			}
			r.cacheMu.Unlock()
		}
	}

	type scoredResult struct {
		chunk indexing.CodeChunk
		score float64
	}

	var candidates []scoredResult

	r.cacheMu.RLock()
	for _, c := range chunks {
		if req.Scope != "" && !strings.HasPrefix(c.Path, req.Scope) {
			continue
		}

		cVec, ok := r.cache[c.ID]
		if !ok {
			continue
		}

		sim := CosineSimilarity(qVec, cVec)
		if sim > 0.01 {
			candidates = append(candidates, scoredResult{
				chunk: c,
				score: sim,
			})
		}
	}
	r.cacheMu.RUnlock()

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	limit := req.TopK
	if len(candidates) < limit {
		limit = len(candidates)
	}

	results := make([]SearchResult, limit)
	for i := 0; i < limit; i++ {
		c := candidates[i].chunk
		results[i] = SearchResult{
			ChunkID:         c.ID,
			Path:            c.Path,
			Language:        c.Language,
			Symbol:          c.Symbol,
			StartLine:       c.StartLine,
			EndLine:         c.EndLine,
			Snippet:         c.Content,
			Score:           candidates[i].score,
			RetrievalSource: "vector",
		}
	}

	return results, nil
}
