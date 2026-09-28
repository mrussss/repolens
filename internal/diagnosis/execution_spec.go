package diagnosis

import (
	"errors"

	"repolens/internal/analysispipeline"
)

type IssueSpec struct {
	Title       string
	Description string
	ErrorLog    string
}

type ProviderSnapshot struct {
	EndpointFingerprint string
	ConfigFingerprint   string
	NormalizedBaseURL   string
	ModelName           string
	TimeoutSeconds      int
	RetryAttempts       int
}

type GenerationSpec struct {
	PromptVersion   string
	AgentVersion    string
	AgentConfigHash string
	Temperature     float64
	ReasoningEffort string
	MaxOutputTokens int
}

type AgentBudget struct {
	MaxAgentRounds         int
	MaxToolCalls           int
	MaxSearchCalls         int
	MaxRepeatCalls         int
	MaxEvidencePacketBytes int
	MaxToolResultBytes     int
	FinalizationTurns      int
}

// DiagnosisExecutionSpec contains only values frozen on the persisted run.
// Credentials remain outside this value so the same endpoint can rotate keys.
type DiagnosisExecutionSpec struct {
	RunID      string
	Lineage    analysispipeline.ResolvedLineage
	Issue      IssueSpec
	Provider   ProviderSnapshot
	Generation GenerationSpec
	Budget     AgentBudget
}

func BuildExecutionSpec(run *DiagnosisRun) (DiagnosisExecutionSpec, error) {
	if run == nil {
		return DiagnosisExecutionSpec{}, errors.New("diagnosis run is nil")
	}
	return DiagnosisExecutionSpec{
		RunID: run.ID,
		Lineage: analysispipeline.ResolvedLineage{
			RepositoryID:        run.RepositoryID,
			RevisionID:          run.AnalysisRevisionID,
			SnapshotID:          run.SnapshotID,
			CodeIndexBuildID:    run.CodeIndexBuildID,
			RetrievalBuildID:    run.RetrievalBuildID,
			PipelineFingerprint: run.PipelineFingerprint,
		},
		Issue: IssueSpec{Title: run.IssueTitle, Description: run.IssueDescription, ErrorLog: run.ErrorLog},
		Provider: ProviderSnapshot{
			EndpointFingerprint: run.ProviderEndpointFingerprint,
			ConfigFingerprint:   run.ProviderConfigFingerprint,
			NormalizedBaseURL:   run.NormalizedBaseURL,
			ModelName:           run.ModelName,
			TimeoutSeconds:      run.ProviderTimeoutSeconds,
			RetryAttempts:       run.ProviderRetryAttempts,
		},
		Generation: GenerationSpec{
			PromptVersion:   run.PromptVersion,
			AgentVersion:    run.AgentVersion,
			AgentConfigHash: run.AgentConfigHash,
			Temperature:     run.Temperature,
			ReasoningEffort: run.ReasoningEffort,
			MaxOutputTokens: run.MaxOutputTokens,
		},
		Budget: AgentBudget{
			MaxAgentRounds:         run.MaxAgentRounds,
			MaxToolCalls:           run.MaxToolCalls,
			MaxSearchCalls:         run.MaxSearchCalls,
			MaxRepeatCalls:         run.MaxRepeatCalls,
			MaxEvidencePacketBytes: run.MaxEvidencePacketBytes,
			MaxToolResultBytes:     run.MaxToolResultBytes,
			FinalizationTurns:      run.FinalizationTurns,
		},
	}, nil
}
