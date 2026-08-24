package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type OpenAICompatibleProvider struct {
	apiKey       string
	baseURL      string
	defaultModel string
	httpClient   *http.Client
}

func NewOpenAICompatibleProvider(apiKey, baseURL, defaultModel string) *OpenAICompatibleProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if defaultModel == "" {
		defaultModel = "gpt-4o"
	}
	return &OpenAICompatibleProvider{
		apiKey:       apiKey,
		baseURL:      baseURL,
		defaultModel: defaultModel,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

type openAIRequest struct {
	Model       string           `json:"model"`
	Messages    []Message        `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

func (p *OpenAICompatibleProvider) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	model := req.Model
	if model == "" {
		model = p.defaultModel
	}

	payload := openAIRequest{
		Model:       model,
		Messages:    req.Messages,
		Tools:       req.Tools,
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return GenerateResponse{}, fmt.Errorf("failed to marshal openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", p.baseURL+"/chat/completions", bytes.NewReader(bodyBytes))
	if err != nil {
		return GenerateResponse{}, fmt.Errorf("failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return GenerateResponse{}, fmt.Errorf("llm http call failed: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return GenerateResponse{}, fmt.Errorf("failed to read llm response body: %w", err)
	}

	if resp.StatusCode == 429 {
		return GenerateResponse{}, fmt.Errorf("rate limit exceeded (429): %s", string(respBytes))
	}
	if resp.StatusCode >= 500 {
		return GenerateResponse{}, fmt.Errorf("provider server error (%d): %s", resp.StatusCode, string(respBytes))
	}
	if resp.StatusCode != http.StatusOK {
		return GenerateResponse{}, fmt.Errorf("llm request failed with status %d: %s", resp.StatusCode, string(respBytes))
	}

	var openAIResp openAIResponse
	if err := json.Unmarshal(respBytes, &openAIResp); err != nil {
		return GenerateResponse{}, fmt.Errorf("failed to unmarshal llm response: %w", err)
	}

	if openAIResp.Error != nil {
		return GenerateResponse{}, fmt.Errorf("llm provider error: %s", openAIResp.Error.Message)
	}
	if len(openAIResp.Choices) == 0 {
		return GenerateResponse{}, fmt.Errorf("empty choices from llm provider")
	}

	choice := openAIResp.Choices[0]
	return GenerateResponse{
		Message:          choice.Message,
		FinishReason:     choice.FinishReason,
		PromptTokens:     openAIResp.Usage.PromptTokens,
		CompletionTokens: openAIResp.Usage.CompletionTokens,
	}, nil
}
