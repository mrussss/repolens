package agent

import (
	"context"
	"testing"

	"repolens/internal/diagnosis"
	"repolens/internal/llm"
)

type finalizationProviderSpy struct {
	toolsLengths []int
}

func (s *finalizationProviderSpy) Generate(_ context.Context, request llm.GenerateRequest) (llm.GenerateResponse, error) {
	s.toolsLengths = append(s.toolsLengths, len(request.Tools))
	return llm.GenerateResponse{Message: llm.Message{
		Role:    llm.RoleAssistant,
		Content: `{"conclusion_kind":"INSUFFICIENT_EVIDENCE","summary":"more evidence is required","limitations":["not enough source evidence"]}`,
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
