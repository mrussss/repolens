package agent

import (
	"context"
	"errors"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/diagnosis"
	"repolens/internal/llm"
	"repolens/internal/tools"
	"repolens/internal/trace"
)

type traceSequenceProvider struct {
	calls int
	fail  bool
}

func (p *traceSequenceProvider) Generate(context.Context, llm.GenerateRequest) (llm.GenerateResponse, error) {
	p.calls++
	if p.fail {
		return llm.GenerateResponse{}, errors.New("provider failed")
	}
	if p.calls == 1 {
		call := llm.ToolCall{ID: "tool-call", Type: "function"}
		call.Function.Name = "search_code"
		call.Function.Arguments = `{"query":"main"}`
		return llm.GenerateResponse{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}}, FinishReason: "tool_calls"}, nil
	}
	return llm.GenerateResponse{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}`},
		FinishReason: "stop",
	}, nil
}

func TestAgentLoopPersistsFinalAndErrorAfterExecutionSteps(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:agent-trace-order?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&trace.AgentStep{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE UNIQUE INDEX uq_step_attempt_seq ON agent_steps (attempt_id, seq)").Error; err != nil {
		t.Fatal(err)
	}
	store := trace.NewStore(db)
	spec := testExecutionSpec(&diagnosis.DiagnosisRun{ID: "run-trace", SnapshotID: "snapshot", IssueTitle: "issue"})

	provider := &traceSequenceProvider{}
	registry := NewToolRegistry()
	registry.Register(tools.NewSearchCodeTool(&assemblyRetriever{}, "snapshot"))
	loop := NewAgentLoop(provider, registry, store, DefaultGuardConfig())
	if _, err := loop.Run(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "attempt-success"}); err != nil {
		t.Fatal(err)
	}
	success, err := store.ListByAttempt(context.Background(), "attempt-success")
	if err != nil {
		t.Fatal(err)
	}
	want := []trace.StepType{trace.StepTypeThinking, trace.StepTypeToolCall, trace.StepTypeToolResult, trace.StepTypeThinking, trace.StepTypeFinalOutput}
	if len(success) != len(want) {
		t.Fatalf("success trace = %+v, want %v", success, want)
	}
	for i, step := range success {
		if step.Seq != i+1 || step.StepType != want[i] {
			t.Fatalf("success trace[%d] = seq %d, %s; want seq %d, %s", i, step.Seq, step.StepType, i+1, want[i])
		}
	}

	failureLoop := NewAgentLoop(&traceSequenceProvider{fail: true}, NewToolRegistry(), store, DefaultGuardConfig())
	if _, err := failureLoop.Run(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "attempt-error"}); err == nil {
		t.Fatal("provider error was not returned")
	}
	failure, err := store.ListByAttempt(context.Background(), "attempt-error")
	if err != nil {
		t.Fatal(err)
	}
	if len(failure) != 2 || failure[0].Seq != 1 || failure[0].StepType != trace.StepTypeThinking || failure[1].Seq != 2 || failure[1].StepType != trace.StepTypeError {
		t.Fatalf("failure trace = %+v", failure)
	}
}
