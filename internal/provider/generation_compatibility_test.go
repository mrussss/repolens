package provider_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"repolens/internal/provider"
)

func TestAssessGenerationCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, effort    string
		tokens, timeout int
		codes           []string
	}{
		{"default", "low", 4096, 60, nil},
		{"medium", "medium", 4096, 60, []string{provider.WarningHigherReasoningBudgetRisk, provider.WarningCustomGenerationProfile}},
		{"high custom", "high", 8192, 120, []string{provider.WarningHigherReasoningBudgetRisk, provider.WarningCustomGenerationProfile}},
		{"empty", "", 4096, 60, []string{provider.WarningCustomGenerationProfile}},
		{"provider specific", "x-provider-effort", 4096, 60, []string{provider.WarningHigherReasoningBudgetRisk, provider.WarningCustomGenerationProfile}},
		{"custom tokens", "low", 1024, 60, []string{provider.WarningCustomGenerationProfile}},
		{"custom timeout", "low", 4096, 30, []string{provider.WarningCustomGenerationProfile}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings := provider.AssessGenerationCompatibility(tc.effort, tc.tokens, tc.timeout)
			if len(warnings) != len(tc.codes) {
				t.Fatalf("warnings=%+v; codes=%v", warnings, tc.codes)
			}
			for i, warning := range warnings {
				if warning.Code != tc.codes[i] || warning.Message == "" {
					t.Fatalf("warning=%+v", warning)
				}
			}
		})
	}
}

func TestGenerationWarningsPreserveProbeRequestsAndOutcomes(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "capability rejection"}[reject], func(t *testing.T) {
			t.Setenv("REPOLENS_REASONING_EFFORT", "high")
			t.Setenv("REPOLENS_MAX_OUTPUT_TOKENS", "8192")
			t.Setenv("REPOLENS_PROVIDER_TIMEOUT_SECONDS", "120")
			var requests []map[string]interface{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request map[string]interface{}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					return
				}
				requests = append(requests, request)
				w.Header().Set("Content-Type", "application/json")
				if len(requests) == 1 {
					_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{}}`))
					return
				}
				if reject {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":{"message":"unsupported reasoning_effort; RAW_PROVIDER_SECRET"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"repolens_compatibility_probe","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`))
			}))
			defer server.Close()
			mgr := provider.NewManager("", "", "", "", "")
			_, result, err := mgr.TestConnectionCompatibilityWithAuthMode(context.Background(), server.URL, "test-model", "API_KEY_SECRET", "bearer")
			if reject {
				if !errors.Is(err, provider.ErrProviderCapabilityUnsupported) {
					t.Fatalf("error=%v", err)
				}
			} else if err != nil || result.ProbeStatus != provider.CompatibilityProbeConfirmed {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if len(requests) != 2 || requests[0]["max_tokens"] != float64(32) || requests[1]["max_tokens"] != float64(256) || requests[1]["reasoning_effort"] != "high" {
				t.Fatalf("probe requests changed: %+v", requests)
			}
			if result.ProductionTimeoutSeconds != 120 || result.ProductionMaxOutputTokens != 8192 || len(result.Warnings) != 2 {
				t.Fatalf("missing profile assessment: %+v", result)
			}
			for _, request := range requests {
				if _, ok := request["warnings"]; ok {
					t.Fatal("warning entered provider request")
				}
				if _, ok := request["production_timeout_seconds"]; ok {
					t.Fatal("assessment changed request shape")
				}
			}
			b, _ := json.Marshal(result)
			if strings.Contains(string(b), "API_KEY_SECRET") || strings.Contains(string(b), "RAW_PROVIDER_SECRET") {
				t.Fatal("unsafe compatibility metadata")
			}
		})
	}
}

func TestGenerationWarningHTTPResponseRemainsSafeOnSuccessAndFailure(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Setenv("REPOLENS_REASONING_EFFORT", "medium")
		t.Setenv("REPOLENS_MAX_OUTPUT_TOKENS", "4096")
		t.Setenv("REPOLENS_PROVIDER_TIMEOUT_SECONDS", "60")
		calls := 0
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls++
			w.Header().Set("Content-Type", "application/json")
			if calls == 1 {
				_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{}}`))
				return
			}
			if reject {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"message":"unsupported reasoning_effort; API_KEY_SECRET RAW_PROVIDER_SECRET"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"choices":[{"message":{"tool_calls":[{"type":"function","function":{"name":"repolens_compatibility_probe","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{}}`))
		}))
		handler := provider.NewHandler(provider.NewManager("", "", "", "", ""), nil, nil, nil, nil, nil, nil, nil)
		router := gin.New()
		router.POST("/settings/provider/test", handler.TestConnection)
		response := performProviderRequestTo(router, http.MethodPost, "/settings/provider/test", `{"base_url":"`+server.URL+`","model":"test-model","api_key":"API_KEY_SECRET"}`)
		server.Close()
		wantStatus := 200
		if reject {
			wantStatus = 502
		}
		if response.Code != wantStatus {
			t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
		}
		var body struct {
			Success       bool                              `json:"success"`
			Compatibility provider.CompatibilityProbeResult `json:"compatibility"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Success == reject || body.Compatibility.ProductionTimeoutSeconds != 60 || len(body.Compatibility.Warnings) != 2 || calls != 2 {
			t.Fatalf("response changed probe semantics: %+v calls=%d", body, calls)
		}
		for _, secret := range []string{"API_KEY_SECRET", "RAW_PROVIDER_SECRET", "unsupported reasoning_effort"} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("HTTP response leaked %s", secret)
			}
		}
	}
}
