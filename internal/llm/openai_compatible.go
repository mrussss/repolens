package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"repolens/internal/platform/redaction"
)

const (
	MaxProviderResponseBytes     int64 = 16 << 20
	maxProviderErrorBodyBytes          = 8 << 10
	ProviderResponseTooLargeCode       = "PROVIDER_RESPONSE_TOO_LARGE"
	OutcomeUnknownErrorCode            = "PROVIDER_OUTCOME_UNKNOWN"
)

// OutcomeUnknownError marks a provider request that may have been dispatched
// but did not yield a complete response. Replaying it automatically could
// repeat billable provider work.
type OutcomeUnknownError struct {
	Cause error
}

func (e *OutcomeUnknownError) Error() string {
	if e == nil || e.Cause == nil {
		return "provider outcome is unknown"
	}
	return fmt.Sprintf("provider outcome is unknown: %v", e.Cause)
}

func (e *OutcomeUnknownError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type OpenAICompatibleProvider struct {
	apiKey           string
	baseURL          string
	defaultModel     string
	authMode         string
	httpClient       *http.Client
	maxResponseBytes int64
}

type ProviderResponseTooLargeError struct {
	LimitBytes int64
}

func (e *ProviderResponseTooLargeError) Error() string {
	return ProviderResponseTooLargeCode
}

func (e *ProviderResponseTooLargeError) ErrorCode() string { return ProviderResponseTooLargeCode }
func (e *ProviderResponseTooLargeError) Permanent() bool   { return true }

type HTTPError struct {
	StatusCode int
	Body       string
	Cause      error
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("llm provider returned HTTP %d: %s", e.StatusCode, redaction.RedactSecrets(e.Body))
}

func (e *HTTPError) RetryableProviderError() bool {
	return e.StatusCode == http.StatusTooManyRequests || (e.StatusCode >= 500 && e.StatusCode <= 599)
}

func (e *HTTPError) HTTPStatusCode() int { return e.StatusCode }

func (e *HTTPError) Unwrap() error { return e.Cause }

func NewOpenAICompatibleProvider(apiKey, baseURL, defaultModel string) *OpenAICompatibleProvider {
	return NewOpenAICompatibleProviderWithAuthMode(apiKey, baseURL, defaultModel, "bearer")
}

func NewOpenAICompatibleProviderWithAuthMode(apiKey, baseURL, defaultModel, authMode string) *OpenAICompatibleProvider {
	return NewOpenAICompatibleProviderWithAuthModeAndTimeout(apiKey, baseURL, defaultModel, authMode, 60*time.Second)
}

func NewOpenAICompatibleProviderWithAuthModeAndTimeout(apiKey, baseURL, defaultModel, authMode string, timeout time.Duration) *OpenAICompatibleProvider {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}
	if defaultModel == "" {
		defaultModel = "gpt-4o"
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	return &OpenAICompatibleProvider{
		apiKey:           apiKey,
		baseURL:          baseURL,
		defaultModel:     defaultModel,
		authMode:         normalizeAuthMode(authMode),
		maxResponseBytes: MaxProviderResponseBytes,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

type openAIRequest struct {
	Model           string           `json:"model"`
	Messages        []Message        `json:"messages"`
	Tools           []ToolDefinition `json:"tools,omitempty"`
	Temperature     *float64         `json:"temperature,omitempty"`
	MaxTokens       int              `json:"max_tokens,omitempty"`
	ReasoningEffort string           `json:"reasoning_effort,omitempty"`
	ResponseFormat  *ResponseFormat  `json:"response_format,omitempty"`
}

type openAIResponse struct {
	Choices []struct {
		Message      *Message `json:"message"`
		FinishReason string   `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
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
		Model:           model,
		Messages:        req.Messages,
		Tools:           req.Tools,
		Temperature:     req.Temperature,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: req.ReasoningEffort,
		ResponseFormat:  req.ResponseFormat,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return GenerateResponse{}, fmt.Errorf("failed to marshal openai request: %w", err)
	}

	endpoint := strings.TrimRight(p.baseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return GenerateResponse{}, fmt.Errorf("failed to create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" && p.authMode == "bearer" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		providerErr := fmt.Errorf("llm http call failed: %w", err)
		if !definitelyPreDispatch(err) {
			return GenerateResponse{}, &OutcomeUnknownError{Cause: providerErr}
		}
		return GenerateResponse{}, providerErr
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		errorBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxProviderErrorBodyBytes+1))
		if int64(len(errorBody)) > maxProviderErrorBodyBytes {
			errorBody = append(errorBody[:maxProviderErrorBodyBytes], []byte("...[truncated]")...)
		}
		return GenerateResponse{}, &HTTPError{
			StatusCode: resp.StatusCode,
			Body:       redaction.RedactSecrets(string(errorBody)),
			Cause:      readErr,
		}
	}
	maxBytes := p.maxResponseBytes
	if maxBytes <= 0 {
		maxBytes = MaxProviderResponseBytes
	}
	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return GenerateResponse{}, &OutcomeUnknownError{Cause: fmt.Errorf("failed to read llm response body after HTTP %d: %w", resp.StatusCode, err)}
	}
	if int64(len(respBytes)) > maxBytes {
		return GenerateResponse{}, &ProviderResponseTooLargeError{LimitBytes: maxBytes}
	}

	var openAIResp openAIResponse
	if err := json.Unmarshal(respBytes, &openAIResp); err != nil {
		return GenerateResponse{}, &OutcomeUnknownError{Cause: fmt.Errorf("failed to decode provider response envelope after HTTP %d: %w", resp.StatusCode, err)}
	}

	if openAIResp.Error != nil {
		return GenerateResponse{}, &OutcomeUnknownError{Cause: fmt.Errorf("provider returned an error envelope after HTTP %d: %s", resp.StatusCode, redaction.RedactSecrets(openAIResp.Error.Message))}
	}
	if len(openAIResp.Choices) == 0 {
		return GenerateResponse{}, &OutcomeUnknownError{Cause: fmt.Errorf("provider response envelope after HTTP %d has no choices", resp.StatusCode)}
	}

	choice := openAIResp.Choices[0]
	if choice.Message == nil {
		return GenerateResponse{}, &OutcomeUnknownError{Cause: fmt.Errorf("provider response envelope after HTTP %d has no message", resp.StatusCode)}
	}
	return GenerateResponse{
		Message:            *choice.Message,
		FinishReason:       choice.FinishReason,
		PromptTokens:       openAIResp.Usage.PromptTokens,
		CompletionTokens:   openAIResp.Usage.CompletionTokens,
		CachedPromptTokens: openAIResp.Usage.PromptTokensDetails.CachedTokens,
		ReasoningTokens:    openAIResp.Usage.CompletionTokensDetails.ReasoningTokens,
	}, nil
}

func definitelyPreDispatch(err error) bool {
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "dial" {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(opErr.Err, &dnsErr)
}

func normalizeAuthMode(mode string) string {
	if mode == "none" {
		return "none"
	}
	return "bearer"
}
