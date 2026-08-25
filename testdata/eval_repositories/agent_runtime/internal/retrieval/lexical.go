package retrieval

import (
	"context"
	"sort"
	"strings"
	"sync"

	"repolens/internal/indexing"
)

type ChunkIndexStore interface {
	GetChunks(snapshotID string) []indexing.CodeChunk
	SaveChunks(snapshotID string, chunks []indexing.CodeChunk)
}

type MemoryChunkStore struct {
	mu     sync.RWMutex
	chunks map[string][]indexing.CodeChunk
}

func NewMemoryChunkStore() *MemoryChunkStore {
	return &MemoryChunkStore{
		chunks: make(map[string][]indexing.CodeChunk),
	}
}

func (s *MemoryChunkStore) GetChunks(snapshotID string) []indexing.CodeChunk {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.chunks[snapshotID]
}

func (s *MemoryChunkStore) SaveChunks(snapshotID string, chunks []indexing.CodeChunk) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks[snapshotID] = chunks
}

func (s *MemoryChunkStore) IndexChunks(ctx context.Context, snapshotID string, chunks []indexing.CodeChunk) error {
	s.SaveChunks(snapshotID, chunks)
	return nil
}

type LexicalRetriever struct {
	chunkStore ChunkIndexStore
}

func NewLexicalRetriever(chunkStore ChunkIndexStore) *LexicalRetriever {
	return &LexicalRetriever{chunkStore: chunkStore}
}

func (r *LexicalRetriever) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	chunks := r.chunkStore.GetChunks(req.SnapshotID)
	if len(chunks) == 0 {
		return nil, nil
	}

	if req.TopK <= 0 {
		req.TopK = 10
	}

	queryLower := strings.ToLower(strings.TrimSpace(req.Query))
	queryTokens := strings.Fields(queryLower)

	type scoredResult struct {
		chunk indexing.CodeChunk
		score float64
	}

	var candidates []scoredResult

	for _, c := range chunks {
		if req.Scope != "" && !strings.HasPrefix(c.Path, req.Scope) {
			continue
		}

		contentLower := strings.ToLower(c.Content)
		pathLower := strings.ToLower(c.Path)
		symbolLower := strings.ToLower(c.Symbol)

		score := 0.0

		// Exact phrase match in content
		if strings.Contains(contentLower, queryLower) {
			score += 10.0
		}
		// Exact symbol match
		if symbolLower != "" && strings.Contains(symbolLower, queryLower) {
			score += 15.0
		}
		// Exact path match
		if strings.Contains(pathLower, queryLower) {
			score += 8.0
		}

		// Token matches
		matchCount := 0
		for _, tok := range queryTokens {
			if strings.Contains(contentLower, tok) {
				score += 2.0
				matchCount++
			}
			if strings.Contains(pathLower, tok) {
				score += 3.0
			}
			if strings.Contains(symbolLower, tok) {
				score += 5.0
			}
		}

		if score > 0 {
			candidates = append(candidates, scoredResult{
				chunk: c,
				score: score,
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
			RetrievalSource: "lexical",
		}
	}

	return results, nil
}
