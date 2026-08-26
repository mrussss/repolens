package indexing

import (
	"context"
)

type ChunkIndexWriter interface {
	IndexChunks(ctx context.Context, snapshotID string, chunks []CodeChunk) error
}
