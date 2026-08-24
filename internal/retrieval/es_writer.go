package retrieval

import (
	"context"
	"fmt"

	"repolens/internal/indexing"
	"repolens/internal/platform/elasticsearch"
)

// ElasticsearchChunkIndexWriter writes repository chunks and dense embeddings into Elasticsearch 8
type ElasticsearchChunkIndexWriter struct {
	client   *elasticsearch.Client
	embedder EmbeddingProvider
}

func NewElasticsearchChunkIndexWriter(client *elasticsearch.Client, embedder EmbeddingProvider) *ElasticsearchChunkIndexWriter {
	return &ElasticsearchChunkIndexWriter{
		client:   client,
		embedder: embedder,
	}
}

func (w *ElasticsearchChunkIndexWriter) IndexChunks(ctx context.Context, snapshotID string, chunks []indexing.CodeChunk) error {
	if len(chunks) == 0 {
		return nil
	}

	dim := 128
	if w.embedder != nil {
		dim = w.embedder.Dimension()
	}

	// 1. Ensure Elasticsearch Index exists with correct dense_vector mapping
	if err := w.client.EnsureIndex(ctx, dim); err != nil {
		return fmt.Errorf("failed to ensure ES index mapping: %w", err)
	}

	// 2. Generate embedding vectors
	var vectors [][]float32
	if w.embedder != nil {
		contents := make([]string, len(chunks))
		for i, ch := range chunks {
			contents[i] = ch.Content
		}
		vecs, err := w.embedder.Embed(ctx, contents)
		if err != nil {
			return fmt.Errorf("failed to compute embeddings for chunks: %w", err)
		}
		vectors = vecs
	}

	// 3. Bulk index chunks into Elasticsearch
	if err := w.client.BulkIndexChunks(ctx, chunks, vectors); err != nil {
		return fmt.Errorf("failed to bulk index chunks into Elasticsearch: %w", err)
	}

	return nil
}

// CompositeChunkIndexWriter dispatches chunk indexing to multiple writers (e.g. Memory + ES)
type CompositeChunkIndexWriter struct {
	writers []indexing.ChunkIndexWriter
}

func NewCompositeChunkIndexWriter(writers ...indexing.ChunkIndexWriter) *CompositeChunkIndexWriter {
	return &CompositeChunkIndexWriter{writers: writers}
}

func (c *CompositeChunkIndexWriter) IndexChunks(ctx context.Context, snapshotID string, chunks []indexing.CodeChunk) error {
	for _, w := range c.writers {
		if err := w.IndexChunks(ctx, snapshotID, chunks); err != nil {
			return err
		}
	}
	return nil
}
