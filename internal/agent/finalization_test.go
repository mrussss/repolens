package agent

import (
	"context"
	"errors"
	"testing"

	"repolens/internal/diagnosis"
	"repolens/internal/llm"
)

type finalizationProviderSpy struct {
	toolsLengths []int
}

type failingFinalizationProvider struct{}

func (failingFinalizationProvider) Generate(context.Context, llm.GenerateRequest) (llm.GenerateResponse, error) {
	return llm.GenerateResponse{}, &llm.CallError{Err: errors.New("provider unavailable"), Attempts: 2}
}

func (s *finalizationProviderSpy) Generate(_ context.Context, request llm.GenerateRequest) (llm.GenerateResponse, error) {
	s.toolsLengths = append(s.toolsLengths, len(request.Tools))
	return llm.GenerateResponse{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: `{"conclusion_kind":"INSUFFICIENT_EVIDENCE","summary":"more evidence is required","confirmed_facts":["the available evidence is incomplete"],"limitations":["not enough source evidence"],"recommended_checks":["collect the missing source evidence"]}`,
	}}, nil
}

func TestFinalizeOnlyDoesNotExposeTools(t *testing.T) {
	spy := &finalizationProviderSpy{}
	loop := NewAgentLoop(spy, NewToolRegistry(), nil, DefaultGuardConfig())
	result, err := loop.finalizeOnly(
		context.Background(),
		&diagnosis.DiagnosisRun{Temperature: 0.1},
		&diagnosis.DiagnosisAttempt{ID: "attempt-finalize"},
		[]llm.Message{{Role: llm.RoleUser, Content: "evidence"}},
		"TOOL_BUDGET", 1, 2, 3, 4, 5, 6, nil, 1, 1,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Report == nil || len(spy.toolsLengths) != 1 || spy.toolsLengths[0] != 0 {
		t.Fatalf("finalization result/tools = %+v/%v, want one request with no tools", result, spy.toolsLengths)
	}
}

func TestFinalizeOnlyReturnsProgressWhenProviderFails(t *testing.T) {
	loop := NewAgentLoop(failingFinalizationProvider{}, NewToolRegistry(), nil, DefaultGuardConfig())
	result, err := loop.finalizeOnly(
		context.Background(),
		&diagnosis.DiagnosisRun{Temperature: 0.1},
		&diagnosis.DiagnosisAttempt{ID: "attempt-finalize-error"},
		[]llm.Message{{Role: llm.RoleUser, Content: "evidence"}},
		"AGENT_ROUND_BUDGET", 10, 20, 3, 4, 2, 1, []string{"search_code"}, 2, 3,
	)
	if err == nil {
		t.Fatal("expected finalization provider error")
	}
	if result == nil || result.PromptTokens != 10 || result.CompletionTokens != 20 || result.ToolCallsCount != 2 || result.ProviderCalls != 4 {
		t.Fatalf("partial finalization result = %+v, want prior progress and two finalization attempts", result)
	}
}
