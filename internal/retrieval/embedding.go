package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

type EmbeddingProvider interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Model() string
	Dimension() int
}

// OpenAICompatibleEmbeddingProvider calls standard OpenAI embedding endpoint POST /v1/embeddings
type OpenAICompatibleEmbeddingProvider struct {
	apiKey    string
	baseURL   string
	model     string
	dimension int
	client    *http.Client
}

func NewOpenAICompatibleEmbeddingProvider(apiKey, baseURL, model string, dim int) *OpenAICompatibleEmbeddingProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	baseURL = strings.TrimSuffix(baseURL, "/")
	if model == "" {
		model = "text-embedding-3-small"
	}
	if dim <= 0 {
		dim = 1536
	}
	return &OpenAICompatibleEmbeddingProvider{
		apiKey:    apiKey,
		baseURL:   baseURL,
		model:     model,
		dimension: dim,
		client:    &http.Client{Timeout: 30 * time.Second},
	}
}

func (p *OpenAICompatibleEmbeddingProvider) Model() string {
	return p.model
}

func (p *OpenAICompatibleEmbeddingProvider) Dimension() int {
	return p.dimension
}

func (p *OpenAICompatibleEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	reqBody := map[string]interface{}{
		"model": p.model,
		"input": texts,
	}
	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := p.baseURL + "/embeddings"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("embedding request failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read embedding response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("embedding API error (status %d): %s", resp.StatusCode, string(respBytes))
	}

	var parsed struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(respBytes, &parsed); err != nil {
		return nil, fmt.Errorf("failed to unmarshal embedding response: %w", err)
	}

	result := make([][]float32, len(texts))
	for _, item := range parsed.Data {
		if item.Index >= 0 && item.Index < len(result) {
			result[item.Index] = item.Embedding
		}
	}
	return result, nil
}

// LocalTFIDFEmbeddingProvider computes deterministic sub-word and token feature embedding vectors
// with L2 normalization for local / offline experimentation
type LocalTFIDFEmbeddingProvider struct {
	model     string
	dimension int
}

func NewLocalTFIDFEmbeddingProvider(dimension int) *LocalTFIDFEmbeddingProvider {
	if dimension <= 0 {
		dimension = 128
	}
	return &LocalTFIDFEmbeddingProvider{
		model:     "local-tfidf-v1",
		dimension: dimension,
	}
}

func (p *LocalTFIDFEmbeddingProvider) Model() string {
	return p.model
}

func (p *LocalTFIDFEmbeddingProvider) Dimension() int {
	return p.dimension
}

func (p *LocalTFIDFEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for i, text := range texts {
		vec := make([]float32, p.dimension)
		tokens := TokenizeCode(text)
		if len(tokens) == 0 {
			result[i] = vec
			continue
		}

		tf := make(map[string]float32)
		for _, tok := range tokens {
			tf[tok] += 1.0
		}

		for tok, count := range tf {
			h := 0
			for _, r := range tok {
				h = (h*31 + int(r)) % p.dimension
			}
			weight := (1.0 + float32(math.Log(float64(count)))) * float32(math.Log(1.0+float64(len(tok))))
			vec[h] += weight
		}

		var norm float32
		for _, v := range vec {
			norm += v * v
		}
		norm = float32(math.Sqrt(float64(norm)))
		if norm > 0 {
			for j := range vec {
				vec[j] /= norm
			}
		}
		result[i] = vec
	}
	return result, nil
}
