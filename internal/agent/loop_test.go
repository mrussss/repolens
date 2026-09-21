package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"repolens/internal/diagnosis"
	"repolens/internal/llm"
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
		Message:      llm.Message{Role: llm.RoleAssistant, Content: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[]}`},
		FinishReason: "stop",
	}, nil
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
	return loop.Run(context.Background(), &diagnosis.DiagnosisRun{
		ID: "run-loop-response", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "issue", IssueDescription: "description", ErrorLog: "error",
	}, &diagnosis.DiagnosisAttempt{ID: "attempt-loop-response"})
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
		Message:          llm.Message{Role: llm.RoleAssistant, Content: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[]}`},
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
	if err != nil || invalid == nil || invalid.StructuredReport || invalid.ParseError == "MODEL_OUTPUT_TRUNCATED" {
		t.Fatalf("invalid stop response took truncation path: result=%+v err=%v", invalid, err)
	}
}

func TestAgentLoopGenerationOptionsReachProviderAndDefaultRemainsCompatible(t *testing.T) {
	defaultProvider := &generationOptionsProvider{}
	defaultLoop := NewAgentLoop(defaultProvider, NewToolRegistry(), nil, DefaultGuardConfig())
	if _, err := defaultLoop.Run(context.Background(), &diagnosis.DiagnosisRun{IssueTitle: "issue"}, &diagnosis.DiagnosisAttempt{ID: "attempt-default-options"}); err != nil {
		t.Fatal(err)
	}
	if len(defaultProvider.requests) != 1 || defaultProvider.requests[0].ReasoningEffort != "" || defaultProvider.requests[0].ResponseFormat == nil || defaultProvider.requests[0].ResponseFormat.Type != "json_object" {
		t.Fatalf("default generation request changed: %+v", defaultProvider.requests)
	}

	experimentProvider := &generationOptionsProvider{}
	experimentLoop := NewAgentLoop(experimentProvider, NewToolRegistry(), nil, DefaultGuardConfig()).WithGenerationOptions(GenerationOptions{
		ReasoningEffort: "low",
		ResponseFormat:  nil,
	})
	if _, err := experimentLoop.Run(context.Background(), &diagnosis.DiagnosisRun{IssueTitle: "issue"}, &diagnosis.DiagnosisAttempt{ID: "attempt-experiment-options"}); err != nil {
		t.Fatal(err)
	}
	if len(experimentProvider.requests) != 1 || experimentProvider.requests[0].ReasoningEffort != "low" || experimentProvider.requests[0].ResponseFormat != nil {
		t.Fatalf("experiment generation request was not propagated: %+v", experimentProvider.requests)
	}
}

func TestParseReportJSONSkipsProseBracesBeforeFencedJSON(t *testing.T) {
	raw := "Analysis note: if shouldRedirect { shouldRedirect = false }\n\n" +
		"```json\n" +
		`{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[]}` +
		"\n```"

	report, err := parseReportJSON(raw)
	if err != nil {
		t.Fatalf("parseReportJSON failed: %v", err)
	}
	if report.RootCause != "root cause" {
		t.Fatalf("root cause = %q, want root cause", report.RootCause)
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
