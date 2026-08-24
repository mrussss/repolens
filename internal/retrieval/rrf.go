package retrieval

import (
	"context"
	"sort"
)

type HybridRRFRetriever struct {
	retrievers []Retriever
	k          float64
}

func NewHybridRRFRetriever(k float64, retrievers ...Retriever) *HybridRRFRetriever {
	if k <= 0 {
		k = 60.0
	}
	return &HybridRRFRetriever{
		retrievers: retrievers,
		k:          k,
	}
}

func (h *HybridRRFRetriever) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	if len(h.retrievers) == 0 {
		return nil, nil
	}

	rrfScores := make(map[string]float64)
	docMap := make(map[string]SearchResult)

	for _, retriever := range h.retrievers {
		results, err := retriever.Search(ctx, req)
		if err != nil {
			continue
		}

		for rank, res := range results {
			key := res.Path + ":" + res.ChunkID
			if _, exists := docMap[key]; !exists {
				docMap[key] = res
			}
			// RRF formula: 1.0 / (k + rank + 1)
			rrfScores[key] += 1.0 / (h.k + float64(rank+1))
		}
	}

	type rankedDoc struct {
		key   string
		score float64
	}

	var ranked []rankedDoc
	for key, score := range rrfScores {
		ranked = append(ranked, rankedDoc{key: key, score: score})
	}

	sort.Slice(ranked, func(i, j int) bool {
		return ranked[i].score > ranked[j].score
	})

	topK := req.TopK
	if topK <= 0 {
		topK = 10
	}
	if len(ranked) < topK {
		topK = len(ranked)
	}

	fused := make([]SearchResult, topK)
	for i := 0; i < topK; i++ {
		res := docMap[ranked[i].key]
		res.Score = ranked[i].score
		res.RetrievalSource = "hybrid_rrf"
		fused[i] = res
	}

	return fused, nil
}
