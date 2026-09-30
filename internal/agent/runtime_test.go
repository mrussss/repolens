package agent

import (
	"context"
	"testing"

	"repolens/internal/diagnosis"
	"repolens/internal/llm"
	"repolens/internal/retrieval"
)

type runtimeGenerationProvider struct {
	requests []llm.GenerateRequest
}

func testExecutionSpec(run *diagnosis.DiagnosisRun) diagnosis.DiagnosisExecutionSpec {
	frozen := *run
	guard := DefaultGuardConfig()
	if frozen.PromptVersion == "" {
		frozen.PromptVersion = diagnosis.CurrentPromptVersion
	}
	if frozen.AgentVersion == "" {
		frozen.AgentVersion = diagnosis.CurrentAgentVersion
	}
	if frozen.MaxAgentRounds == 0 {
		frozen.MaxAgentRounds = guard.MaxSteps
	}
	if frozen.MaxToolCalls == 0 {
		frozen.MaxToolCalls = guard.MaxToolCalls
	}
	if frozen.MaxSearchCalls == 0 {
		frozen.MaxSearchCalls = guard.MaxSearchCalls
	}
	if frozen.MaxRepeatCalls == 0 {
		frozen.MaxRepeatCalls = guard.MaxRepeatCalls
	}
	if frozen.MaxEvidencePacketBytes == 0 {
		frozen.MaxEvidencePacketBytes = 32 * 1024
	}
	if frozen.MaxToolResultBytes == 0 {
		frozen.MaxToolResultBytes = guard.MaxToolResultBytes
	}
	if frozen.FinalizationTurns == 0 {
		frozen.FinalizationTurns = 1
	}
	if frozen.MaxOutputTokens == 0 {
		frozen.MaxOutputTokens = guard.MaxOutputTokens
	}
	if frozen.ProviderTimeoutSeconds == 0 {
		frozen.ProviderTimeoutSeconds = 60
	}
	frozen.AgentConfigHash = diagnosis.ComputeAgentConfigHashWithGenerationOptions(
		frozen.MaxAgentRounds, frozen.MaxToolCalls, frozen.MaxSearchCalls, frozen.MaxRepeatCalls,
		frozen.MaxEvidencePacketBytes, frozen.MaxToolResultBytes, frozen.FinalizationTurns,
		frozen.MaxOutputTokens, frozen.ProviderTimeoutSeconds, frozen.ProviderRetryAttempts,
		frozen.Temperature, frozen.ReasoningEffort, "json_object",
	)
	spec, err := diagnosis.BuildExecutionSpec(&frozen)
	if err != nil {
		panic(err)
	}
	return spec
}

func (p *runtimeGenerationProvider) Generate(_ context.Context, request llm.GenerateRequest) (llm.GenerateResponse, error) {
	p.requests = append(p.requests, request)
	return llm.GenerateResponse{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}`},
		FinishReason: "stop",
	}, nil
}

func TestRuntimeUsesFrozenRunReasoningEffortAndSupportsEmptyValue(t *testing.T) {
	for _, want := range []string{"low", ""} {
		t.Run("reasoning="+want, func(t *testing.T) {
			provider := &runtimeGenerationProvider{}
			executor := NewAgentRuntimeExecutor(provider, nil, nil, nil, DefaultGuardConfig())
			run := &diagnosis.DiagnosisRun{ID: "run-runtime-" + want, IssueTitle: "issue", ReasoningEffort: want}
			if _, err := executor.Execute(context.Background(), testExecutionSpec(run), &diagnosis.DiagnosisAttempt{ID: "attempt-runtime-" + want}); err != nil {
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
		if _, err := executor.Execute(context.Background(), testExecutionSpec(run), &diagnosis.DiagnosisAttempt{ID: attemptID}); err != nil {
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

func TestRuntimeUsesFrozenGenerationBudgetAndInitialRetrieval(t *testing.T) {
	provider := &runtimeGenerationProvider{}
	retriever := &assemblyRetriever{}
	executor := NewAgentRuntimeExecutor(provider, retriever, nil, nil, DefaultGuardConfig())
	spec := testExecutionSpec(&diagnosis.DiagnosisRun{
		ID: "run-runtime-spec", Temperature: 0.25, ReasoningEffort: "low", MaxOutputTokens: 1024,
		MaxAgentRounds: 3, MaxToolCalls: 4, MaxSearchCalls: 2, MaxRepeatCalls: 1,
		MaxToolResultBytes: 8192,
	})
	spec.Lineage.RepositoryID = "repo"
	spec.Lineage.SnapshotID = "snapshot"
	spec.Lineage.CodeIndexBuildID = 17
	spec.Lineage.RetrievalBuildID = 23
	spec.Issue.Title = "crash"
	spec.Issue.Description = "handler failure"
	result, err := executor.Execute(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "attempt-runtime-spec"})
	if err != nil {
		t.Fatal(err)
	}
	if len(retriever.requests) != 1 {
		t.Fatalf("initial retrieval requests = %+v", retriever.requests)
	}
	request := retriever.requests[0]
	if request.TopK != 8 || request.Query != retrieval.BuildQuery("crash", "handler failure", "") || request.SnapshotID != "snapshot" || request.CodeIndexBuildID != 17 || request.RetrievalBuildID != 23 {
		t.Fatalf("initial retrieval request = %+v", request)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider requests = %+v", provider.requests)
	}
	generated := provider.requests[0]
	if len(generated.Tools) != 5 || generated.MaxTokens != 1024 || generated.ReasoningEffort != "low" || generated.Temperature == nil || *generated.Temperature != 0.25 || generated.ResponseFormat == nil || generated.ResponseFormat.Type != "json_object" {
		t.Fatalf("provider request changed: %+v", generated)
	}
	if result.ProviderCalls != 1 || result.FinishReason != "stop" || !result.StructuredReport || result.Report == nil || result.Report.RootCause != "root cause" {
		t.Fatalf("execution result changed: %+v", result)
	}
}
