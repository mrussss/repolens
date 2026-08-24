package elasticsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"repolens/internal/indexing"
	"repolens/internal/platform/logger"
)

type ESChunkDoc struct {
	SnapshotID string    `json:"snapshot_id"`
	ChunkID    string    `json:"chunk_id"`
	Path       string    `json:"path"`
	Language   string    `json:"language"`
	Symbol     string    `json:"symbol,omitempty"`
	StartLine  int       `json:"start_line"`
	EndLine    int       `json:"end_line"`
	Content    string    `json:"content"`
	Embedding  []float32 `json:"embedding,omitempty"`
}

type Client struct {
	baseURL    string
	indexName  string
	httpClient *http.Client
}

func NewClient(baseURL, indexName string) *Client {
	if baseURL == "" {
		baseURL = "http://localhost:9200"
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	if indexName == "" {
		indexName = "repolens_chunks"
	}
	return &Client{
		baseURL:    baseURL,
		indexName:  indexName,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) Ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL, nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to ping elasticsearch at %s: %w", c.baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("elasticsearch ping returned status %d", resp.StatusCode)
	}
	return nil
}

func (c *Client) EnsureIndex(ctx context.Context, embeddingDim int) error {
	if embeddingDim <= 0 {
		embeddingDim = 128
	}

	// Check if index exists
	headReq, err := http.NewRequestWithContext(ctx, http.MethodHead, c.baseURL+"/"+c.indexName, nil)
	if err != nil {
		return err
	}
	headResp, err := c.httpClient.Do(headReq)
	if err == nil && headResp.StatusCode == http.StatusOK {
		headResp.Body.Close()
		return nil
	}
	if headResp != nil {
		headResp.Body.Close()
	}

	// Create index with mapping
	mapping := map[string]interface{}{
		"mappings": map[string]interface{}{
			"properties": map[string]interface{}{
				"snapshot_id": map[string]interface{}{"type": "keyword"},
				"chunk_id":    map[string]interface{}{"type": "keyword"},
				"path":        map[string]interface{}{"type": "keyword"},
				"language":    map[string]interface{}{"type": "keyword"},
				"symbol":      map[string]interface{}{"type": "keyword"},
				"start_line":  map[string]interface{}{"type": "integer"},
				"end_line":    map[string]interface{}{"type": "integer"},
				"content":     map[string]interface{}{"type": "text", "analyzer": "standard"},
				"embedding": map[string]interface{}{
					"type":       "dense_vector",
					"dims":       embeddingDim,
					"index":      true,
					"similarity": "cosine",
				},
			},
		},
	}
	bodyBytes, err := json.Marshal(mapping)
	if err != nil {
		return err
	}

	putReq, err := http.NewRequestWithContext(ctx, http.MethodPut, c.baseURL+"/"+c.indexName, bytes.NewReader(bodyBytes))
	if err != nil {
		return err
	}
	putReq.Header.Set("Content-Type", "application/json")

	putResp, err := c.httpClient.Do(putReq)
	if err != nil {
		return fmt.Errorf("failed to create elasticsearch index: %w", err)
	}
	defer putResp.Body.Close()

	if putResp.StatusCode != http.StatusOK && putResp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(putResp.Body)
		return fmt.Errorf("failed to create index (status %d): %s", putResp.StatusCode, string(respBody))
	}

	logger.L(ctx).Info("ensured elasticsearch index", "index", c.indexName, "dims", embeddingDim)
	return nil
}

func (c *Client) BulkIndexChunks(ctx context.Context, chunks []indexing.CodeChunk, embeddings [][]float32) error {
	if len(chunks) == 0 {
		return nil
	}

	var buf bytes.Buffer
	for i, ch := range chunks {
		meta := map[string]interface{}{
			"index": map[string]interface{}{
				"_index": c.indexName,
				"_id":    ch.ID,
			},
		}
		metaJSON, _ := json.Marshal(meta)
		buf.Write(metaJSON)
		buf.WriteString("\n")

		doc := ESChunkDoc{
			SnapshotID: ch.SnapshotID,
			ChunkID:    ch.ID,
			Path:       ch.Path,
			Language:   ch.Language,
			Symbol:     ch.Symbol,
			StartLine:  ch.StartLine,
			EndLine:    ch.EndLine,
			Content:    ch.Content,
		}
		if i < len(embeddings) && len(embeddings[i]) > 0 {
			doc.Embedding = embeddings[i]
		}
		docJSON, _ := json.Marshal(doc)
		buf.Write(docJSON)
		buf.WriteString("\n")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/_bulk?refresh=true", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-ndjson")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to bulk index into elasticsearch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bulk index error (status %d): %s", resp.StatusCode, string(respBody))
	}
	return nil
}

type ESSearchHit struct {
	ID     string     `json:"_id"`
	Score  float64    `json:"_score"`
	Source ESChunkDoc `json:"_source"`
}

type ESSearchResponse struct {
	Hits struct {
		Total struct {
			Value int `json:"value"`
		} `json:"total"`
		Hits []ESSearchHit `json:"hits"`
	} `json:"hits"`
}

func (c *Client) SearchBM25(ctx context.Context, snapshotID, query, scope string, topK int) ([]ESSearchHit, error) {
	if topK <= 0 {
		topK = 10
	}

	mustClauses := []map[string]interface{}{
		{
			"multi_match": map[string]interface{}{
				"query":  query,
				"fields": []string{"content^1.0", "symbol^3.0", "path^2.0"},
			},
		},
	}

	filterClauses := []map[string]interface{}{
		{"term": map[string]interface{}{"snapshot_id": snapshotID}},
	}
	if scope != "" {
		filterClauses = append(filterClauses, map[string]interface{}{
			"prefix": map[string]interface{}{"path": scope},
		})
	}

	searchBody := map[string]interface{}{
		"size": topK,
		"query": map[string]interface{}{
			"bool": map[string]interface{}{
				"must":   mustClauses,
				"filter": filterClauses,
			},
		},
	}

	return c.executeSearch(ctx, searchBody)
}

func (c *Client) SearchKNN(ctx context.Context, snapshotID string, queryVector []float32, scope string, topK int) ([]ESSearchHit, error) {
	if topK <= 0 {
		topK = 10
	}

	filterClauses := []map[string]interface{}{
		{"term": map[string]interface{}{"snapshot_id": snapshotID}},
	}
	if scope != "" {
		filterClauses = append(filterClauses, map[string]interface{}{
			"prefix": map[string]interface{}{"path": scope},
		})
	}

	searchBody := map[string]interface{}{
		"size": topK,
		"knn": map[string]interface{}{
			"field":          "embedding",
			"query_vector":   queryVector,
			"k":              topK,
			"num_candidates": topK * 5,
			"filter":         filterClauses,
		},
	}

	return c.executeSearch(ctx, searchBody)
}

func (c *Client) executeSearch(ctx context.Context, body map[string]interface{}) ([]ESSearchHit, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+c.indexName+"/_search", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("elasticsearch search query failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read search response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("elasticsearch error (status %d): %s", resp.StatusCode, string(respBytes))
	}

	var searchResp ESSearchResponse
	if err := json.Unmarshal(respBytes, &searchResp); err != nil {
		return nil, fmt.Errorf("failed to decode search response: %w", err)
	}

	return searchResp.Hits.Hits, nil
}
