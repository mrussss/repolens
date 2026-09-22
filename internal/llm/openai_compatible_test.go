package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func float64Ptr(value float64) *float64 {
	return &value
}
