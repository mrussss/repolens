package agent

import (
	"context"
	"testing"

	"repolens/internal/diagnosis"
)

// VALIDATION-ONLY: convert these observations to desired-invariant regression
// tests during production hardening.
func TestValidationFC08IncompatibleFrozenIdentityReachesProvider(t *testing.T) {
	currentConfigHash := diagnosis.ComputeAgentConfigHashWithGenerationOptions(
		8, 12, 3, 2, 32*1024, 32*1024, 1, 4096, 60, 0, 0, "", "json_object",
	)
	cases := []struct {
		name   string
		mutate func(*diagnosis.DiagnosisRun)
	}{
		{name: "old prompt version", mutate: func(run *diagnosis.DiagnosisRun) { run.PromptVersion = "old-prompt" }},
		{name: "old agent version", mutate: func(run *diagnosis.DiagnosisRun) { run.AgentVersion = "old-agent" }},
		{name: "incompatible agent config hash", mutate: func(run *diagnosis.DiagnosisRun) { run.AgentConfigHash = "incompatible" }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := &diagnosis.DiagnosisRun{
				ID: "validation-fc08-" + tc.name, IssueTitle: "FC-08 frozen execution identity",
				PromptVersion: diagnosis.CurrentPromptVersion, AgentVersion: diagnosis.CurrentAgentVersion,
				AgentConfigHash: currentConfigHash,
			}
			tc.mutate(run)
			spec, err := diagnosis.BuildExecutionSpec(run)
			if err != nil {
				t.Fatal(err)
			}
			provider := &runtimeGenerationProvider{}
			executor := NewAgentRuntimeExecutor(provider, nil, nil, nil, DefaultGuardConfig())
			result, err := executor.Execute(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "validation-fc08-attempt"})
			if err != nil {
				t.Fatalf("runtime rejected the frozen execution spec: %v", err)
			}
			if len(provider.requests) == 0 || result == nil || result.ProviderCalls == 0 {
				t.Fatalf("provider boundary was not reached: calls=%d result=%+v", len(provider.requests), result)
			}
			t.Logf("observation: incompatible %q spec reached provider; provider calls=%d", tc.name, result.ProviderCalls)
		})
	}
}
