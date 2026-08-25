package retrieval_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"repolens/internal/indexing"
	"repolens/internal/platform/elasticsearch"
	"repolens/internal/retrieval"
)

func createTestChunks(snapshotID string) []indexing.CodeChunk {
	return []indexing.CodeChunk{
		{
			ID:         "c1",
			SnapshotID: snapshotID,
			Path:       "internal/auth/jwt.go",
			Language:   "go",
			Symbol:     "ValidateToken",
			StartLine:  10,
			EndLine:    30,
			Content:    "func ValidateToken(tokenStr string) (*Claims, error) {\n    // Parse token with secret key and check expiration\n    return parseJWT(tokenStr)\n}",
		},
		{
			ID:         "c2",
			SnapshotID: snapshotID,
			Path:       "internal/db/mysql.go",
			Language:   "go",
			Symbol:     "ConnectMySQL",
			StartLine:  1,
			EndLine:    25,
			Content:    "func ConnectMySQL(dsn string) (*sql.DB, error) {\n    // Connect to database pool and ping\n    return sql.Open(\"mysql\", dsn)\n}",
		},
		{
			ID:         "c3",
			SnapshotID: snapshotID,
			Path:       "internal/worker/consumer.go",
			Language:   "go",
			Symbol:     "HandleMessage",
			StartLine:  40,
			EndLine:    80,
			Content:    "func HandleMessage(msg amqp.Delivery) {\n    // Process message with rate limiting and 429 retry backoff\n    handleTask(msg)\n}",
		},
	}
}

func TestBM25Retriever(t *testing.T) {
	chunkStore := retrieval.NewMemoryChunkStore()
	snapID := "snap-bm25-test"
	chunks := createTestChunks(snapID)
	chunkStore.SaveChunks(snapID, chunks)

	bm25 := retrieval.NewBM25Retriever(chunkStore)
	ctx := context.Background()

	// Query for database connection
	res, err := bm25.Search(ctx, retrieval.SearchRequest{
		SnapshotID: snapID,
		Query:      "ConnectMySQL database dsn",
		TopK:       2,
	})
	if err != nil {
		t.Fatalf("BM25 search failed: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("expected at least 1 result")
	}
	if res[0].Path != "internal/db/mysql.go" {
		t.Errorf("expected top result to be internal/db/mysql.go, got %s", res[0].Path)
	}
	if res[0].RetrievalSource != "bm25" {
		t.Errorf("expected retrieval source bm25, got %s", res[0].RetrievalSource)
	}
}

func TestVectorRetrieverWithLocalTFIDFEmbedding(t *testing.T) {
	chunkStore := retrieval.NewMemoryChunkStore()
	snapID := "snap-vector-test"
	chunks := createTestChunks(snapID)
	chunkStore.SaveChunks(snapID, chunks)

	embedder := retrieval.NewLocalTFIDFEmbeddingProvider(128)
	vecRetriever := retrieval.NewVectorRetriever(chunkStore, embedder)
	ctx := context.Background()

	res, err := vecRetriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: snapID,
		Query:      "jwt token expiration validate",
		TopK:       2,
	})
	if err != nil {
		t.Fatalf("Vector search failed: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("expected at least 1 result from vector search")
	}
	if res[0].Path != "internal/auth/jwt.go" {
		t.Errorf("expected top result to be jwt.go, got %s", res[0].Path)
	}
	if res[0].RetrievalSource != "vector" {
		t.Errorf("expected retrieval source vector, got %s", res[0].RetrievalSource)
	}
}

func TestHybridRRFRetriever(t *testing.T) {
	chunkStore := retrieval.NewMemoryChunkStore()
	snapID := "snap-rrf-test"
	chunks := createTestChunks(snapID)
	chunkStore.SaveChunks(snapID, chunks)

	bm25 := retrieval.NewBM25Retriever(chunkStore)
	embedder := retrieval.NewLocalTFIDFEmbeddingProvider(128)
	vector := retrieval.NewVectorRetriever(chunkStore, embedder)

	hybrid := retrieval.NewHybridRRFRetriever(60, bm25, vector)
	ctx := context.Background()

	res, err := hybrid.Search(ctx, retrieval.SearchRequest{
		SnapshotID: snapID,
		Query:      "rate limiting 429 retry",
		TopK:       3,
	})
	if err != nil {
		t.Fatalf("Hybrid search failed: %v", err)
	}
	if len(res) == 0 {
		t.Fatalf("expected results from hybrid RRF")
	}
	if res[0].Path != "internal/worker/consumer.go" {
		t.Errorf("expected top result internal/worker/consumer.go, got %s", res[0].Path)
	}
	if res[0].RetrievalSource != "hybrid_rrf" {
		t.Errorf("expected retrieval source hybrid_rrf, got %s", res[0].RetrievalSource)
	}
}

func TestElasticsearchClientAndRetriever(t *testing.T) {
	// Mock Elasticsearch HTTP Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.13.0"}}`))
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && (r.URL.Path == "/_bulk" || r.URL.Path == "/repolens_chunks/_bulk"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errors":false,"items":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repolens_chunks/_search":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"hits": {
					"total": {"value": 1},
					"hits": [
						{
							"_id": "c1",
							"_score": 2.5,
							"_source": {
								"snapshot_id": "snap-1",
								"chunk_id": "c1",
								"path": "internal/auth/jwt.go",
								"language": "go",
								"symbol": "ValidateToken",
								"start_line": 10,
								"end_line": 30,
								"content": "func ValidateToken() {}"
							}
						}
					]
				}
			}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	esClient := elasticsearch.NewClient(server.URL, "repolens_chunks")
	ctx := context.Background()

	if err := esClient.Ping(ctx); err != nil {
		t.Fatalf("ping failed: %v", err)
	}

	esBM25 := retrieval.NewESBM25Retriever(esClient)
	results, err := esBM25.Search(ctx, retrieval.SearchRequest{
		SnapshotID: "snap-1",
		Query:      "jwt token",
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("ES BM25 search failed: %v", err)
	}
	if len(results) != 1 || results[0].Path != "internal/auth/jwt.go" {
		t.Errorf("unexpected ES search results: %+v", results)
	}

	embedder := retrieval.NewLocalTFIDFEmbeddingProvider(128)
	esVector := retrieval.NewESVectorRetriever(esClient, embedder)
	vecResults, err := esVector.Search(ctx, retrieval.SearchRequest{
		SnapshotID: "snap-1",
		Query:      "token validation",
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("ES Vector search failed: %v", err)
	}
	if len(vecResults) != 1 || vecResults[0].RetrievalSource != "es_vector" {
		t.Errorf("unexpected ES vector results: %+v", vecResults)
	}
}
