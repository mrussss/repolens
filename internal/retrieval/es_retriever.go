package retrieval

import (
	"context"
	"fmt"

	"repolens/internal/platform/elasticsearch"
)

type ESBM25Retriever struct {
	client *elasticsearch.Client
}

func NewESBM25Retriever(client *elasticsearch.Client) *ESBM25Retriever {
	return &ESBM25Retriever{client: client}
}

func (r *ESBM25Retriever) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	hits, err := r.client.SearchBM25(ctx, req.SnapshotID, req.Query, req.Scope, req.TopK)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch BM25 search failed: %w", err)
	}

	results := make([]SearchResult, len(hits))
	for i, hit := range hits {
		results[i] = SearchResult{
			ChunkID:         hit.Source.ChunkID,
			Path:            hit.Source.Path,
			Language:        hit.Source.Language,
			Symbol:          hit.Source.Symbol,
			StartLine:       hit.Source.StartLine,
			EndLine:         hit.Source.EndLine,
			Snippet:         hit.Source.Content,
			Score:           hit.Score,
			RetrievalSource: "es_bm25",
		}
	}
	return results, nil
}

type ESVectorRetriever struct {
	client   *elasticsearch.Client
	embedder EmbeddingProvider
}

func NewESVectorRetriever(client *elasticsearch.Client, embedder EmbeddingProvider) *ESVectorRetriever {
	if embedder == nil {
		embedder = NewLocalTFIDFEmbeddingProvider(128)
	}
	return &ESVectorRetriever{
		client:   client,
		embedder: embedder,
	}
}

func (r *ESVectorRetriever) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	vecs, err := r.embedder.Embed(ctx, []string{req.Query})
	if err != nil {
		return nil, fmt.Errorf("failed to embed query for elasticsearch vector search: %w", err)
	}
	if len(vecs) == 0 || len(vecs[0]) == 0 {
		return nil, nil
	}

	hits, err := r.client.SearchKNN(ctx, req.SnapshotID, vecs[0], req.Scope, req.TopK)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch vector kNN search failed: %w", err)
	}

	results := make([]SearchResult, len(hits))
	for i, hit := range hits {
		results[i] = SearchResult{
			ChunkID:         hit.Source.ChunkID,
			Path:            hit.Source.Path,
			Language:        hit.Source.Language,
			Symbol:          hit.Source.Symbol,
			StartLine:       hit.Source.StartLine,
			EndLine:         hit.Source.EndLine,
			Snippet:         hit.Source.Content,
			Score:           hit.Score,
			RetrievalSource: "es_vector",
		}
	}
	return results, nil
}
