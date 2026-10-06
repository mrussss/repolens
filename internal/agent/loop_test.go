package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"repolens/internal/diagnosis"
	"repolens/internal/llm"
	providersettings "repolens/internal/provider"
	"repolens/internal/trace"
)

type loopResponseProvider struct {
	response llm.GenerateResponse
}

func (p loopResponseProvider) Generate(context.Context, llm.GenerateRequest) (llm.GenerateResponse, error) {
	return p.response, nil
}

type generationOptionsProvider struct {
	requests []llm.GenerateRequest
}

func (p *generationOptionsProvider) Generate(_ context.Context, request llm.GenerateRequest) (llm.GenerateResponse, error) {
	p.requests = append(p.requests, request)
	return llm.GenerateResponse{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}`},
		FinishReason: "stop",
	}, nil
}

type failingTraceStore struct {
	calls atomic.Int32
}

func (s *failingTraceStore) Create(context.Context, *trace.AgentStep) error {
	s.calls.Add(1)
	return errors.New("injected trace persistence failure")
}

func (*failingTraceStore) ListByAttempt(context.Context, string) ([]trace.AgentStep, error) {
	return nil, nil
}

func (*failingTraceStore) ListAfterSeq(context.Context, string, int) ([]trace.AgentStep, error) {
	return nil, nil
}

func TestAgentLoopTracePersistenceFailureIsBestEffort(t *testing.T) {
	provider := &generationOptionsProvider{}
	traceStore := &failingTraceStore{}
	loop := NewAgentLoop(provider, NewToolRegistry(), traceStore, DefaultGuardConfig())
	run := &diagnosis.DiagnosisRun{ID: "run-trace-failure", RepositoryID: "repo", SnapshotID: "snapshot", IssueTitle: "issue"}
	result, err := loop.Run(context.Background(), testExecutionSpec(run), &diagnosis.DiagnosisAttempt{ID: "attempt-trace-failure"})
	if err != nil || result == nil || !result.StructuredReport {
		t.Fatalf("trace persistence failure changed Agent execution: result=%+v err=%v", result, err)
	}
	if traceStore.calls.Load() < 2 {
		t.Fatalf("trace writes attempted=%d; want THINKING and FINAL_OUTPUT attempts", traceStore.calls.Load())
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider calls=%d after trace persistence failure; want one", len(provider.requests))
	}
}

func TestProviderPromptContainsOnlyRedactedPersistedDiagnosisInput(t *testing.T) {
	provider := &generationOptionsProvider{}
	loop := NewAgentLoop(provider, NewToolRegistry(), nil, DefaultGuardConfig())
	run := &diagnosis.DiagnosisRun{
		ID: "run-redacted-prompt", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "issue", IssueDescription: `{"password":"supersecret123"}`,
		ErrorLog: `{"password": "supersecret123"}`,
	}
	if _, err := loop.Run(context.Background(), testExecutionSpec(run), &diagnosis.DiagnosisAttempt{ID: "attempt-redacted-prompt"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider calls = %d, want one", len(provider.requests))
	}
	for _, message := range provider.requests[0].Messages {
		if strings.Contains(message.Content, "supersecret123") {
			t.Fatalf("provider-visible prompt contains original credential: %s", message.Content)
		}
	}
}

func runLoopResponseTest(t *testing.T, response llm.GenerateResponse, maxOutputTokens int) (*LoopResult, error) {
	t.Helper()
	loop := NewAgentLoop(loopResponseProvider{response: response}, NewToolRegistry(), nil, GuardConfig{
		MaxSteps:        2,
		MaxToolCalls:    2,
		MaxSearchCalls:  1,
		MaxRepeatCalls:  1,
		MaxOutputTokens: maxOutputTokens,
	})
	return loop.Run(context.Background(), testExecutionSpec(&diagnosis.DiagnosisRun{
		ID: "run-loop-response", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "issue", IssueDescription: "description", ErrorLog: "error",
	}), &diagnosis.DiagnosisAttempt{ID: "attempt-loop-response"})
}

func TestAgentLoopRejectsLengthTruncationBeforeParsing(t *testing.T) {
	result, err := runLoopResponseTest(t, llm.GenerateResponse{
		Message:          llm.Message{Role: llm.RoleAssistant},
		FinishReason:     "length",
		CompletionTokens: 100,
		ReasoningTokens:  100,
	}, 2048)
	if err == nil || !strings.Contains(err.Error(), "MODEL_OUTPUT_TRUNCATED") {
		t.Fatalf("expected MODEL_OUTPUT_TRUNCATED, result=%+v err=%v", result, err)
	}
	if result == nil || result.StructuredReport {
		t.Fatalf("truncated response should not be structured: %+v", result)
	}
	if result.FinishReason != "length" || result.ParseError != ErrCodeModelOutputTruncated {
		t.Fatalf("truncation evidence = finish_reason=%q parse_error=%q", result.FinishReason, result.ParseError)
	}
}

type reasoningTruncationProvider struct{ requests []llm.GenerateRequest }

func (p *reasoningTruncationProvider) Generate(_ context.Context, req llm.GenerateRequest) (llm.GenerateResponse, error) {
	p.requests = append(p.requests, req)
	return llm.GenerateResponse{Message: llm.Message{Role: llm.RoleAssistant}, FinishReason: "length", CompletionTokens: 4096, ReasoningTokens: 4000}, nil
}

func TestGenerationWarningDoesNotRetryOrDowngradeReasoningTruncation(t *testing.T) {
	warnings := providersettings.AssessGenerationCompatibility("high", 4096, 60)
	if len(warnings) != 2 {
		t.Fatal("expected advisory warnings")
	}
	p := &reasoningTruncationProvider{}
	cfg := DefaultGuardConfig()
	loop := NewAgentLoop(p, NewToolRegistry(), nil, cfg).WithGenerationOptions(GenerationOptions{ReasoningEffort: "high"})
	run := &diagnosis.DiagnosisRun{ID: "warning-truncation", RepositoryID: "repo", SnapshotID: "snapshot", IssueTitle: "issue", ReasoningEffort: "high", MaxOutputTokens: 4096, ProviderTimeoutSeconds: 60}
	spec := testExecutionSpec(run)
	before, _ := json.Marshal(spec)
	result, err := loop.Run(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "warning-attempt"})
	after, _ := json.Marshal(spec)
	if !errors.Is(err, ErrModelOutputTruncated) || result == nil || result.ParseError != ErrCodeModelOutputTruncated || result.ReasoningTokens != 4000 || result.ProviderCalls != 1 {
		t.Fatalf("truncation boundary changed: %+v %v", result, err)
	}
	if len(p.requests) != 1 || p.requests[0].ReasoningEffort != "high" || p.requests[0].MaxTokens != 4096 || string(before) != string(after) {
		t.Fatalf("warning changed request, snapshot, or retry: %+v", p.requests)
	}
}

func TestAgentLoopDetectsBudgetExhaustionWithoutFinishReason(t *testing.T) {
	_, err := runLoopResponseTest(t, llm.GenerateResponse{
		Message:          llm.Message{Role: llm.RoleAssistant},
		CompletionTokens: 2048,
	}, 2048)
	if err == nil || !strings.Contains(err.Error(), "MODEL_OUTPUT_TRUNCATED") {
		t.Fatalf("expected budget exhaustion truncation, got %v", err)
	}
}

func TestAgentLoopKeepsNormalStopAndInvalidJSONSeparateFromTruncation(t *testing.T) {
	valid, err := runLoopResponseTest(t, llm.GenerateResponse{
		Message:          llm.Message{Role: llm.RoleAssistant, Content: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}`},
		FinishReason:     "stop",
		CompletionTokens: 100,
	}, 2048)
	if err != nil || valid == nil || !valid.StructuredReport {
		t.Fatalf("valid stop response was not accepted: result=%+v err=%v", valid, err)
	}
	if valid.FinishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop", valid.FinishReason)
	}

	invalid, err := runLoopResponseTest(t, llm.GenerateResponse{
		Message:          llm.Message{Role: llm.RoleAssistant, Content: "not json"},
		FinishReason:     "stop",
		CompletionTokens: 100,
	}, 2048)
	if err == nil || !errors.Is(err, ErrInvalidStructuredReport) || invalid == nil || invalid.StructuredReport || invalid.ParseError == ErrCodeModelOutputTruncated || invalid.RawOutput != "not json" {
		t.Fatalf("invalid stop response did not take invalid structured path: result=%+v err=%v", invalid, err)
	}
}

func TestParseReportJSONRejectsInvalidStructuredReports(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{name: "malformed json", raw: "{not-json"},
		{name: "unknown field", raw: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}],"unexpected":true}`},
		{name: "trailing json", raw: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}{}`},
		{name: "missing required fields", raw: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[]}`},
		{name: "illegal conclusion kind", raw: `{"conclusion_kind":"MAYBE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}`},
		{name: "incomplete insufficient evidence", raw: `{"conclusion_kind":"INSUFFICIENT_EVIDENCE","confirmed_facts":[]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseReportJSON(tt.raw)
			if !errors.Is(err, ErrInvalidStructuredReport) {
				t.Fatalf("parse error = %v, want %s", err, ErrCodeInvalidStructuredReport)
			}
			if !strings.Contains(err.Error(), ErrCodeInvalidStructuredReport) {
				t.Fatalf("parse error = %v, want stable error code", err)
			}
		})
	}
}

func TestAgentLoopGenerationOptionsReachProviderAndDefaultRemainsCompatible(t *testing.T) {
	defaultProvider := &generationOptionsProvider{}
	defaultLoop := NewAgentLoop(defaultProvider, NewToolRegistry(), nil, DefaultGuardConfig())
	if _, err := defaultLoop.Run(context.Background(), testExecutionSpec(&diagnosis.DiagnosisRun{IssueTitle: "issue"}), &diagnosis.DiagnosisAttempt{ID: "attempt-default-options"}); err != nil {
		t.Fatal(err)
	}
	if len(defaultProvider.requests) != 1 || defaultProvider.requests[0].ReasoningEffort != "" || defaultProvider.requests[0].ResponseFormat == nil || defaultProvider.requests[0].ResponseFormat.Type != "json_object" {
		t.Fatalf("default generation request changed: %+v", defaultProvider.requests)
	}
	if !strings.Contains(defaultProvider.requests[0].Messages[0].Content, "JSON response contract") {
		t.Fatalf("JSON-mode request lacks explicit JSON instruction: %+v", defaultProvider.requests[0].Messages[0])
	}

	experimentProvider := &generationOptionsProvider{}
	experimentLoop := NewAgentLoop(experimentProvider, NewToolRegistry(), nil, DefaultGuardConfig()).WithGenerationOptions(GenerationOptions{
		ReasoningEffort: "low",
		ResponseFormat:  nil,
	})
	if _, err := experimentLoop.Run(context.Background(), testExecutionSpec(&diagnosis.DiagnosisRun{IssueTitle: "issue"}), &diagnosis.DiagnosisAttempt{ID: "attempt-experiment-options"}); err != nil {
		t.Fatal(err)
	}
	if len(experimentProvider.requests) != 1 || experimentProvider.requests[0].ReasoningEffort != "low" || experimentProvider.requests[0].ResponseFormat != nil {
		t.Fatalf("experiment generation request was not propagated: %+v", experimentProvider.requests)
	}
}

func TestParseReportJSONRejectsProseAndFencedJSON(t *testing.T) {
	raw := "Analysis note: if shouldRedirect { shouldRedirect = false }\n\n" +
		"```json\n" +
		`{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}` +
		"\n```"

	_, err := parseReportJSON(raw)
	if err == nil || !errors.Is(err, ErrInvalidStructuredReport) {
		t.Fatalf("expected strict JSON-only response rejection, got %v", err)
	}
}

func TestParseReportJSONRejectsUnknownFieldNameLeak(t *testing.T) {
	marker := "secret-test-marker"
	raw := `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}],"` + marker + `":true}`
	_, err := parseReportJSON(raw)
	if err == nil || !strings.Contains(err.Error(), "UNKNOWN_FIELD") || strings.Contains(err.Error(), marker) {
		t.Fatalf("parse error must be stable and not expose unknown field name: %v", err)
	}
}

func TestParseReportJSONRequiresOneCompleteObject(t *testing.T) {
	valid := `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}`
	for _, raw := range []string{valid, valid + `{}`, valid + ` garbage`} {
		_, err := parseReportJSON(raw)
		if raw == valid && err != nil {
			t.Fatalf("valid object rejected: %v", err)
		}
		if raw != valid && (err == nil || !errors.Is(err, ErrInvalidStructuredReport)) {
			t.Fatalf("non-single-object response accepted: %q", raw)
		}
	}
}

func TestParseReportJSONAcceptsAttemptScopedEvidenceReference(t *testing.T) {
	report, err := parseReportJSON(`{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning","citations":[{"evidence_id":"ev_123","reason":"supports the finding"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Findings[0].Citations[0].EvidenceID; got != "ev_123" {
		t.Fatalf("evidence ID = %q, want ev_123", got)
	}
}

func TestBoundToolResultDoesNotSplitEvidenceJSON(t *testing.T) {
	result := boundToolResult("read_file", strings.Repeat("source", 20), 16)
	if len(result) > 16 {
		// A structured error is deliberately short but may still exceed an
		// unusually tiny caller limit; it must never contain a partial source.
		if strings.Contains(result, "source") {
			t.Fatalf("oversized result leaked partial source: %q", result)
		}
	}
	if !json.Valid([]byte(result)) {
		t.Fatalf("evidence overflow result is not valid JSON: %q", result)
	}
}

func TestSystemPromptUsesEvidenceIDOnlyForFinalCitations(t *testing.T) {
	for _, field := range []string{`"path"`, `"start_line"`, `"end_line"`, `"excerpt"`, `"content_hash"`} {
		if strings.Contains(SystemPrompt, field) {
			t.Fatalf("system prompt still requests legacy citation field %s", field)
		}
	}
	if !strings.Contains(SystemPrompt, `"evidence_id"`) {
		t.Fatal("system prompt does not define evidence_id citation field")
	}
}

func TestRecordStepRedactsPersistedTraceFields(t *testing.T) {
	store := &recordingTraceStore{}
	loop := &AgentLoop{traceStore: store}
	if err := loop.recordStep(context.Background(), "attempt", 1, trace.StepTypeToolCall,
		"Authorization: Bearer tool-secret",
		`{"Authorization":"Basic args-secret"}`,
		"Authorization: Bearer result-secret", "COMPLETED", 0, 0, 0,
		"Authorization: Basic error-secret", "Authorization: Bearer finish-secret"); err != nil {
		t.Fatal(err)
	}
	for field, value := range map[string]string{
		"tool name": store.step.ToolName, "arguments": store.step.ToolArgsSummary,
		"result": store.step.ToolResultSummary, "error code": store.step.ErrorCode,
		"finish reason": store.step.FinishReason,
	} {
		for _, secret := range []string{"tool-secret", "args-secret", "result-secret", "error-secret", "finish-secret"} {
			if strings.Contains(value, secret) {
				t.Errorf("trace %s retained %s: %q", field, secret, value)
			}
		}
	}
	if !json.Valid([]byte(store.step.ToolArgsSummary)) {
		t.Fatalf("redaction corrupted JSON trace arguments: %q", store.step.ToolArgsSummary)
	}
}

type recordingTraceStore struct{ step *trace.AgentStep }

func (s *recordingTraceStore) Create(_ context.Context, step *trace.AgentStep) error {
	copy := *step
	s.step = &copy
	return nil
}
func (*recordingTraceStore) ListByAttempt(context.Context, string) ([]trace.AgentStep, error) {
	return nil, nil
}
func (*recordingTraceStore) ListAfterSeq(context.Context, string, int) ([]trace.AgentStep, error) {
	return nil, nil
}
