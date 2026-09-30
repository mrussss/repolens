package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAICompatibleTemperatureWireEncoding(t *testing.T) {
	tests := []struct {
		name        string
		temperature *float64
		present     bool
		want        float64
	}{
		{name: "unspecified", present: false},
		{name: "explicit zero", temperature: float64Ptr(0), present: true, want: 0},
		{name: "explicit value", temperature: float64Ptr(0.1), present: true, want: 0.1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var request map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`))
			}))
			defer server.Close()

			_, err := NewOpenAICompatibleProvider("key", server.URL, "model").Generate(context.Background(), GenerateRequest{
				Messages:    []Message{{Role: RoleUser, Content: "ping"}},
				Temperature: tt.temperature,
			})
			if err != nil {
				t.Fatalf("Generate failed: %v", err)
			}

			raw, exists := request["temperature"]
			if exists != tt.present {
				t.Fatalf("temperature presence = %v, want %v", exists, tt.present)
			}
			if exists && raw.(float64) != tt.want {
				t.Fatalf("temperature = %v, want %v", raw, tt.want)
			}
		})
	}
}

func TestOpenAICompatibleReasoningRoundTripAndUsage(t *testing.T) {
	requestBodies := make([]map[string]interface{}, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		requestBodies = append(requestBodies, request)
		w.Header().Set("Content-Type", "application/json")
		if len(requestBodies) == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"internal reasoning state","tool_calls":[{"id":"call-1","type":"function","function":{"name":"search_code","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":2},"completion_tokens_details":{"reasoning_tokens":4}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider("key", server.URL, "model")
	response, err := provider.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: RoleUser, Content: "inspect"}}, ReasoningEffort: "medium", ResponseFormat: &ResponseFormat{Type: "json_object"}})
	if err != nil {
		t.Fatalf("first Generate failed: %v", err)
	}
	if response.Message.ReasoningContent != "internal reasoning state" || len(response.Message.ToolCalls) != 1 {
		t.Fatalf("reasoning/tool call was not preserved: %+v", response.Message)
	}
	if response.CachedPromptTokens != 2 || response.ReasoningTokens != 4 {
		t.Fatalf("extended usage = cached %d, reasoning %d", response.CachedPromptTokens, response.ReasoningTokens)
	}

	_, err = provider.Generate(context.Background(), GenerateRequest{Messages: []Message{
		{Role: RoleUser, Content: "inspect"},
		response.Message,
	}})
	if err != nil {
		t.Fatalf("round-trip Generate failed: %v", err)
	}
	messages := requestBodies[1]["messages"].([]interface{})
	assistant := messages[1].(map[string]interface{})
	if assistant["reasoning_content"] != "internal reasoning state" || assistant["tool_calls"] == nil {
		t.Fatalf("assistant message did not round-trip reasoning/tool calls: %v", assistant)
	}
	if _, present := requestBodies[1]["reasoning_effort"]; present {
		t.Fatalf("empty reasoning_effort was sent: %v", requestBodies[1])
	}
	if requestBodies[0]["reasoning_effort"] != "medium" {
		t.Fatalf("reasoning_effort was not sent: %v", requestBodies[0])
	}
	format := requestBodies[0]["response_format"].(map[string]interface{})
	if format["type"] != "json_object" {
		t.Fatalf("response_format was not sent: %v", format)
	}
}

func TestOpenAICompatibleProductionGenerationShape(t *testing.T) {
	var request map[string]interface{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{}}`))
	}))
	defer server.Close()

	_, err := NewOpenAICompatibleProvider("key", server.URL, "model").Generate(context.Background(), GenerateRequest{
		Messages:        []Message{{Role: RoleUser, Content: "inspect"}},
		Tools:           []ToolDefinition{{Type: "function", Function: ToolFunction{Name: "probe", Description: "probe", Parameters: map[string]interface{}{"type": "object"}}}},
		MaxTokens:       4096,
		ReasoningEffort: "low",
		ResponseFormat:  &ResponseFormat{Type: "json_object"},
	})
	if err != nil {
		t.Fatalf("Generate failed: %v", err)
	}
	if request["max_tokens"] != float64(4096) || request["reasoning_effort"] != "low" {
		t.Fatalf("generation limits were not sent: %v", request)
	}
	format, ok := request["response_format"].(map[string]interface{})
	if !ok || format["type"] != "json_object" {
		t.Fatalf("response format was not sent: %v", request)
	}
	tools, ok := request["tools"].([]interface{})
	if !ok || len(tools) != 1 {
		t.Fatalf("tools were not sent: %v", request)
	}
}

func TestOpenAICompatibleTimeoutDefaultsWhenInvalid(t *testing.T) {
	provider := NewOpenAICompatibleProviderWithAuthModeAndTimeout("key", "http://localhost", "model", "bearer", 0)
	if provider.httpClient.Timeout != 60*time.Second {
		t.Fatalf("timeout = %v, want 60s", provider.httpClient.Timeout)
	}

	provider = NewOpenAICompatibleProviderWithAuthModeAndTimeout("key", "http://localhost", "model", "bearer", 3*time.Second)
	if provider.httpClient.Timeout != 3*time.Second {
		t.Fatalf("timeout = %v, want 3s", provider.httpClient.Timeout)
	}
}

func TestOpenAICompatibleBoundsProviderResponseIndependentlyOfMaxTokens(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 33)))
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProvider("key", server.URL, "model")
	provider.maxResponseBytes = 32
	_, err := provider.Generate(context.Background(), GenerateRequest{MaxTokens: 1})
	var tooLarge *ProviderResponseTooLargeError
	if !errors.As(err, &tooLarge) || tooLarge.ErrorCode() != ProviderResponseTooLargeCode {
		t.Fatalf("error = %v, want %s", err, ProviderResponseTooLargeCode)
	}
}

func TestOpenAICompatibleBoundsAndRedactsProviderErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("Authorization: Bearer short-provider-secret\n" + strings.Repeat("x", 12<<10)))
	}))
	defer server.Close()

	_, err := NewOpenAICompatibleProvider("key", server.URL, "model").Generate(context.Background(), GenerateRequest{})
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want HTTPError", err)
	}
	if strings.Contains(httpErr.Body, "short-provider-secret") || len(httpErr.Body) > int(maxProviderErrorBodyBytes)+32 {
		t.Fatalf("provider error body was not bounded and redacted: bytes=%d body=%q", len(httpErr.Body), httpErr.Body[:min(len(httpErr.Body), 128)])
	}
	if !strings.Contains(httpErr.Body, "[truncated]") {
		t.Fatalf("bounded provider error body is missing truncation marker: %q", httpErr.Body[len(httpErr.Body)-32:])
	}
}

func TestOpenAICompatibleMarksInterruptedSuccessBodyOutcomeUnknown(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "128")
		w.WriteHeader(http.StatusOK)
		if _, err := io.WriteString(w, `{"choices":[`); err != nil {
			t.Errorf("write partial provider response: %v", err)
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack provider response: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	provider := NewOpenAICompatibleProviderWithAuthModeAndTimeout("", server.URL, "model", "none", 3*time.Second)
	_, err := provider.Generate(context.Background(), GenerateRequest{})
	var unknown *OutcomeUnknownError
	if !errors.As(err, &unknown) || unknown.Cause == nil {
		t.Fatalf("interrupted successful response error = %v, want OutcomeUnknownError with cause", err)
	}
}

func TestRetryingProviderPreservesExplicit5xxWhenErrorBodyIsInterrupted(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "128")
			w.WriteHeader(http.StatusServiceUnavailable)
			if _, err := io.WriteString(w, `{"error":`); err != nil {
				t.Errorf("write partial provider error response: %v", err)
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack provider error response: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{}}`)
	}))
	defer server.Close()

	base := NewOpenAICompatibleProviderWithAuthModeAndTimeout("", server.URL, "model", "none", 3*time.Second)
	if _, err := NewRetryingProvider(base, 1).Generate(context.Background(), GenerateRequest{}); err != nil {
		t.Fatalf("provider retry after explicit HTTP 503: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("HTTP request count = %d, want 2 for one explicit 503 retry", requests.Load())
	}
}

func float64Ptr(value float64) *float64 {
	return &value
}
