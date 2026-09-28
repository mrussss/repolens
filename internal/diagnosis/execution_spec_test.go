package diagnosis_test

import (
	"context"
	"reflect"
	"testing"

	"repolens/internal/analysispipeline"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
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

func TestRetryRebuildsEquivalentPersistedExecutionSpec(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	store := diagnosis.NewStore(db)
	jobStore := jobs.NewStoreWithDriver(mustSQLDB(t, db), "sqlite3")
	run := &diagnosis.DiagnosisRun{
		ID: "retry-frozen-spec", UserID: "user", RepositoryID: "repo", AnalysisRevisionID: "revision",
		SnapshotID: "snapshot", CodeIndexBuildID: 11, RetrievalBuildID: 12,
		PipelineFingerprint: "pipeline", IssueTitle: "title", IssueDescription: "description", ErrorLog: "log",
		ProviderEndpointFingerprint: "endpoint", ProviderConfigFingerprint: "config",
		NormalizedBaseURL: "https://provider.example/v1", ModelName: "model",
		ProviderTimeoutSeconds: 31, ProviderRetryAttempts: 2,
		PromptVersion: "prompt", AgentVersion: "agent", AgentConfigHash: "agent-config",
		Temperature: 0.2, ReasoningEffort: "low", MaxOutputTokens: 2048,
		MaxAgentRounds: 8, MaxToolCalls: 12, MaxSearchCalls: 3, MaxRepeatCalls: 2,
		MaxEvidencePacketBytes: 32000, MaxToolResultBytes: 16000, FinalizationTurns: 1,
		IdempotencyKey: "retry-frozen-spec-key", IdempotencyRequestHash: "retry-frozen-spec-hash",
	}
	if err := store.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&diagnosis.DiagnosisRun{}).Where("id = ?", run.ID).Update("status", diagnosis.StatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).Updates(map[string]interface{}{
		"status": jobs.StatusFailed, "last_error_class": jobs.ErrorClassRetryable, "last_error_code": "PROVIDER_TIMEOUT",
	}).Error; err != nil {
		t.Fatal(err)
	}
	before, err := store.GetByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeSpec, err := diagnosis.BuildExecutionSpec(before)
	if err != nil {
		t.Fatal(err)
	}
	svc := diagnosis.NewService(diagnosis.ServiceDependencies{
		Store: store, RepoStore: repo.NewStore(db),
		Lineage:  analysispipeline.NewResolver(revision.NewStore(db), snapshot.NewStore(db), codeintelstore.NewStore(db)),
		JobStore: jobStore,
		ProviderSource: func() diagnosis.ProviderMetadata {
			return diagnosis.ProviderMetadata{IsConfigured: true, ConfigFingerprint: "config"}
		},
	})
	if err := svc.Retry(ctx, run.ID, "user"); err != nil {
		t.Fatalf("real diagnosis retry: %v", err)
	}
	job, err := jobStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil || job == nil || job.Status != jobs.StatusPending || job.ExecutionGeneration != 2 {
		t.Fatalf("retry job = %+v err=%v, want PENDING generation 2", job, err)
	}
	after, err := store.GetByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterSpec, err := diagnosis.BuildExecutionSpec(after)
	if err != nil || !reflect.DeepEqual(afterSpec, beforeSpec) {
		t.Fatalf("execution spec changed after retry: before=%+v after=%+v err=%v", beforeSpec, afterSpec, err)
	}
}
