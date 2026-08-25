package integration_real

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"repolens/internal/indexing"
	"repolens/internal/platform/elasticsearch"
	"repolens/internal/retrieval"
)

func setupRealElasticsearch(t *testing.T) (*elasticsearch.Client, func()) {
	ctx := context.Background()

	// Check if image exists locally to prevent slow network hangs during test runs
	if err := exec.Command("docker", "image", "inspect", "elasticsearch:8.13.0").Run(); err != nil {
		if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "1" {
			t.Fatalf("FAILED: real Elasticsearch image elasticsearch:8.13.0 required by release gate but not present in Docker: %v", err)
		}
		t.Skipf("Skipping real Elasticsearch test (image elasticsearch:8.13.0 not present locally in Docker: %v)", err)
		return nil, nil
	}

	req := tc.ContainerRequest{
		Image:        "elasticsearch:8.13.0",
		ExposedPorts: []string{"9200/tcp"},
		Env: map[string]string{
			"discovery.type":         "single-node",
			"xpack.security.enabled": "false",
			"ES_JAVA_OPTS":           "-Xms512m -Xmx512m",
		},
		WaitingFor: wait.ForHTTP("/").WithPort("9200/tcp").WithStatusCodeMatcher(func(status int) bool {
			return status == 200
		}).WithStartupTimeout(60 * time.Second),
	}

	esContainer, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "1" {
			t.Fatalf("FAILED: real Elasticsearch testcontainers required by release gate but failed to start: %v", err)
		}
		t.Skipf("Skipping real Elasticsearch testcontainers test (Docker not available: %v)", err)
		return nil, nil
	}

	host, err := esContainer.Host(ctx)
	if err != nil {
		_ = esContainer.Terminate(ctx)
		t.Fatalf("failed to get Elasticsearch container host: %v", err)
	}
	port, err := esContainer.MappedPort(ctx, "9200/tcp")
	if err != nil {
		_ = esContainer.Terminate(ctx)
		t.Fatalf("failed to get Elasticsearch mapped port: %v", err)
	}

	esURL := fmt.Sprintf("http://%s:%s", host, port.Port())
	client := elasticsearch.NewClient(esURL, "repolens_real_test_chunks")

	// Wait for ES to be responsive
	var pingErr error
	for i := 0; i < 15; i++ {
		if pingErr = client.Ping(ctx); pingErr == nil {
			break
		}
		time.Sleep(1 * time.Second)
	}
	if pingErr != nil {
		_ = esContainer.Terminate(ctx)
		t.Fatalf("failed to ping real Elasticsearch cluster: %v", pingErr)
	}

	cleanup := func() {
		_ = esContainer.Terminate(context.Background())
	}

	return client, cleanup
}

func TestRealElasticsearch_MappingBulkAndHybridRRF(t *testing.T) {
	client, cleanup := setupRealElasticsearch(t)
	if client == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()

	embedder := retrieval.NewLocalHashedFeatureProvider(128)
	dim := embedder.Dimension()

	// 1. EnsureIndex with dense_vector mapping on real ES 8
	if err := client.EnsureIndex(ctx, dim); err != nil {
		t.Fatalf("failed to create index with dense_vector mapping on real ES: %v", err)
	}

	// 2. Prepare Chunks
	chunks := []indexing.CodeChunk{
		{
			ID:         "chunk-es-001",
			SnapshotID: "snap-real-es-1",
			Path:       "internal/payment/stripe.go",
			Language:   "go",
			Symbol:     "ProcessStripeWebhook",
			StartLine:  15,
			EndLine:    50,
			Content:    "func ProcessStripeWebhook(payload []byte) error {\n    // Verify signature\n    if !verifyStripeSignature(payload) {\n        return errors.New(\"invalid stripe signature\")\n    }\n    return nil\n}",
		},
		{
			ID:         "chunk-es-002",
			SnapshotID: "snap-real-es-1",
			Path:       "internal/payment/paypal.go",
			Language:   "go",
			Symbol:     "HandlePayPalIPN",
			StartLine:  10,
			EndLine:    40,
			Content:    "func HandlePayPalIPN(body []byte) error {\n    // Process paypal instant payment notification\n    return nil\n}",
		},
		{
			ID:         "chunk-es-003",
			SnapshotID: "snap-real-es-1",
			Path:       "internal/auth/jwt.go",
			Language:   "go",
			Symbol:     "ValidateToken",
			StartLine:  1,
			EndLine:    30,
			Content:    "func ValidateToken(tokenStr string) (*Claims, error) {\n    // Validate JWT signature and expiration\n    return parseClaims(tokenStr)\n}",
		},
	}

	contents := []string{chunks[0].Content, chunks[1].Content, chunks[2].Content}
	vectors, err := embedder.Embed(ctx, contents)
	if err != nil {
		t.Fatalf("failed to embed chunk contents: %v", err)
	}

	// 3. Bulk Index Chunks into real Elasticsearch 8
	if err := client.BulkIndexChunks(ctx, chunks, vectors); err != nil {
		t.Fatalf("failed to bulk index chunks into real ES 8: %v", err)
	}

	// Small pause to ensure ES refresh
	time.Sleep(1 * time.Second)

	// 4. Test BM25 Multi-match Search
	bm25Retriever := retrieval.NewESBM25Retriever(client)
	bm25Res, err := bm25Retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: "snap-real-es-1",
		Query:      "verify stripe signature",
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("BM25 search failed on real ES: %v", err)
	}
	if len(bm25Res) == 0 || bm25Res[0].Path != "internal/payment/stripe.go" {
		t.Fatalf("BM25 expected Top-1 to be stripe.go, got: %+v", bm25Res)
	}

	// 5. Test Dense kNN Vector Search
	vectorRetriever := retrieval.NewESVectorRetriever(client, embedder)
	vecRes, err := vectorRetriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: "snap-real-es-1",
		Query:      "ProcessStripeWebhook payload signature",
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("kNN vector search failed on real ES: %v", err)
	}
	if len(vecRes) == 0 || vecRes[0].Path != "internal/payment/stripe.go" {
		t.Fatalf("kNN Vector expected Top-1 to be stripe.go, got: %+v", vecRes)
	}

	// 6. Test Hybrid RRF Search
	hybridRetriever := retrieval.NewHybridRRFRetriever(60, bm25Retriever, vectorRetriever)
	hybridRes, err := hybridRetriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: "snap-real-es-1",
		Query:      "stripe signature webhook",
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("hybrid RRF search failed on real ES: %v", err)
	}
	if len(hybridRes) == 0 || hybridRes[0].Path != "internal/payment/stripe.go" {
		t.Fatalf("Hybrid RRF expected Top-1 to be stripe.go, got: %+v", hybridRes)
	}
	if hybridRes[0].Score <= 0 {
		t.Errorf("expected positive RRF score, got %f", hybridRes[0].Score)
	}
}

func TestRealElasticsearch_ChunkIndexWriter(t *testing.T) {
	client, cleanup := setupRealElasticsearch(t)
	if client == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	embedder := retrieval.NewLocalHashedFeatureProvider(128)
	writer := retrieval.NewElasticsearchChunkIndexWriter(client, embedder)

	chunks := []indexing.CodeChunk{
		{
			ID:         "chunk-writer-001",
			SnapshotID: "snap-writer-es-1",
			Path:       "internal/auth/session.go",
			Language:   "go",
			Symbol:     "CreateSession",
			StartLine:  1,
			EndLine:    20,
			Content:    "func CreateSession(userID string) (string, error) {\n    return generateToken(userID)\n}",
		},
	}

	if err := writer.IndexChunks(ctx, "snap-writer-es-1", chunks); err != nil {
		t.Fatalf("failed to index chunks with ElasticsearchChunkIndexWriter: %v", err)
	}

	time.Sleep(1 * time.Second)

	bm25 := retrieval.NewESBM25Retriever(client)
	res, err := bm25.Search(ctx, retrieval.SearchRequest{
		SnapshotID: "snap-writer-es-1",
		Query:      "CreateSession generateToken",
		TopK:       1,
	})
	if err != nil || len(res) == 0 {
		t.Fatalf("expected to find indexed chunk via ES BM25: err=%v, res=%+v", err, res)
	}
}
