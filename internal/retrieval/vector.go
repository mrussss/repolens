package retrieval

import (
	"context"
	"math"
	"sort"
	"strings"

	"repolens/internal/indexing"
)

type VectorRetriever struct {
	chunkStore ChunkIndexStore
}

func NewVectorRetriever(chunkStore ChunkIndexStore) *VectorRetriever {
	return &VectorRetriever{chunkStore: chunkStore}
}

// PseudoDenseEmbedding computes a deterministic semantic hash vector for testing/experiments
func pseudoEmbed(text string, dim int) []float64 {
	vec := make([]float64, dim)
	tokens := TokenizeCode(text)
	for _, tok := range tokens {
		h := 0
		for _, r := range tok {
			h = (h*31 + int(r)) % dim
		}
		vec[h] += 1.0
	}
	// normalize
	norm := 0.0
	for _, v := range vec {
		norm += v * v
	}
	norm = math.Sqrt(norm)
	if norm > 0 {
		for i := range vec {
			vec[i] /= norm
		}
	}
	return vec
}

func cosineSim(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}
	dot := 0.0
	for i := range a {
		dot += a[i] * b[i]
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

	dim := 128
	qVec := pseudoEmbed(req.Query, dim)

	type scoredResult struct {
		chunk indexing.CodeChunk
		score float64
	}

	var candidates []scoredResult

	for _, c := range chunks {
		if req.Scope != "" && !strings.HasPrefix(c.Path, req.Scope) {
			continue
		}

		cVec := pseudoEmbed(c.Content+" "+c.Symbol+" "+c.Path, dim)
		sim := cosineSim(qVec, cVec)

		if sim > 0.05 {
			candidates = append(candidates, scoredResult{
				chunk: c,
				score: sim,
			})
		}
	}

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
