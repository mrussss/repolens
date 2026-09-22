package agent

import (
	"context"
	"testing"

	"repolens/internal/diagnosis"
	"repolens/internal/llm"
)

type runtimeGenerationProvider struct {
	requests []llm.GenerateRequest
}

func (p *runtimeGenerationProvider) Generate(_ context.Context, request llm.GenerateRequest) (llm.GenerateResponse, error) {
	p.requests = append(p.requests, request)
	return llm.GenerateResponse{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[]}`},
		FinishReason: "stop",
	}, nil
}

func TestRuntimeUsesFrozenRunReasoningEffortAndSupportsEmptyValue(t *testing.T) {
	for _, want := range []string{"low", ""} {
		t.Run("reasoning="+want, func(t *testing.T) {
			provider := &runtimeGenerationProvider{}
			executor := NewAgentRuntimeExecutor(provider, nil, nil, nil, DefaultGuardConfig())
			run := &diagnosis.DiagnosisRun{ID: "run-runtime-" + want, IssueTitle: "issue", ReasoningEffort: want}
			if _, err := executor.Execute(context.Background(), run, &diagnosis.DiagnosisAttempt{ID: "attempt-runtime-" + want}); err != nil {
				t.Fatal(err)
			}
			if len(provider.requests) != 1 || provider.requests[0].ReasoningEffort != want {
				t.Fatalf("reasoning_effort = %q, want %q; requests=%+v", provider.requests[0].ReasoningEffort, want, provider.requests)
			}
		})
	}
}

func TestRuntimeResumeKeepsRunReasoningEffort(t *testing.T) {
	provider := &runtimeGenerationProvider{}
	executor := NewAgentRuntimeExecutor(provider, nil, nil, nil, DefaultGuardConfig())
	run := &diagnosis.DiagnosisRun{ID: "run-runtime-resume", IssueTitle: "issue", ReasoningEffort: "low"}
	for _, attemptID := range []string{"attempt-runtime-1", "attempt-runtime-2"} {
		if _, err := executor.Execute(context.Background(), run, &diagnosis.DiagnosisAttempt{ID: attemptID}); err != nil {
			t.Fatal(err)
		}
	}
	if len(provider.requests) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.requests))
	}
	for i, request := range provider.requests {
		if request.ReasoningEffort != "low" {
			t.Fatalf("request %d reasoning_effort = %q, want low", i, request.ReasoningEffort)
		}
	}
}
