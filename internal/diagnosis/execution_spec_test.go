package diagnosis_test

import (
	"reflect"
	"testing"

	"repolens/internal/analysispipeline"
	"repolens/internal/diagnosis"
)

func TestBuildExecutionSpecMapsFrozenRunFields(t *testing.T) {
	run := &diagnosis.DiagnosisRun{
		ID: "run", UserID: "private-user", RepositoryID: "repo", AnalysisRevisionID: "revision",
		SnapshotID: "snapshot", CodeIndexBuildID: 11, RetrievalBuildID: 12,
		PipelineFingerprint: "pipeline", IssueTitle: "title", IssueDescription: "description", ErrorLog: "log",
		ProviderEndpointFingerprint: "endpoint", ProviderConfigFingerprint: "config",
		NormalizedBaseURL: "https://provider.example/v1", ModelName: "model",
		ProviderTimeoutSeconds: 31, ProviderRetryAttempts: 2,
		PromptVersion: "prompt", AgentVersion: "agent", AgentConfigHash: "agent-config",
		Temperature: 0.2, ReasoningEffort: "low", MaxOutputTokens: 2048,
		MaxAgentRounds: 8, MaxToolCalls: 12, MaxSearchCalls: 3, MaxRepeatCalls: 2,
		MaxEvidencePacketBytes: 32000, MaxToolResultBytes: 16000, FinalizationTurns: 1,
	}
	want := diagnosis.DiagnosisExecutionSpec{
		RunID: "run",
		Lineage: analysispipeline.ResolvedLineage{
			RepositoryID: "repo", RevisionID: "revision", SnapshotID: "snapshot",
			CodeIndexBuildID: 11, RetrievalBuildID: 12, PipelineFingerprint: "pipeline",
		},
		Issue:      diagnosis.IssueSpec{Title: "title", Description: "description", ErrorLog: "log"},
		Provider:   diagnosis.ProviderSnapshot{EndpointFingerprint: "endpoint", ConfigFingerprint: "config", NormalizedBaseURL: "https://provider.example/v1", ModelName: "model", TimeoutSeconds: 31, RetryAttempts: 2},
		Generation: diagnosis.GenerationSpec{PromptVersion: "prompt", AgentVersion: "agent", AgentConfigHash: "agent-config", Temperature: 0.2, ReasoningEffort: "low", MaxOutputTokens: 2048},
		Budget:     diagnosis.AgentBudget{MaxAgentRounds: 8, MaxToolCalls: 12, MaxSearchCalls: 3, MaxRepeatCalls: 2, MaxEvidencePacketBytes: 32000, MaxToolResultBytes: 16000, FinalizationTurns: 1},
	}
	got, err := diagnosis.BuildExecutionSpec(run)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("execution spec = %+v, want %+v", got, want)
	}
	// Retry attempts use the same persisted run, so its execution spec is stable.
	again, err := diagnosis.BuildExecutionSpec(run)
	if err != nil || !reflect.DeepEqual(again, got) {
		t.Fatalf("retry execution spec changed: %+v err=%v", again, err)
	}
	if got.Lineage.CommitSHA != "" || got.Lineage.PipelineVersion != "" {
		t.Fatalf("unpersisted lineage fields were invented: %+v", got.Lineage)
	}
}

func TestBuildExecutionSpecRejectsNilRun(t *testing.T) {
	if _, err := diagnosis.BuildExecutionSpec(nil); err == nil {
		t.Fatal("expected nil run error")
	}
}
