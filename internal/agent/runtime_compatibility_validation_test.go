package agent

import (
	"context"
	"testing"

	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/llm"
)

func TestFC08IncompatibleFrozenIdentityFailsBeforeProviderFactory(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(*diagnosis.DiagnosisRun)
		wantCode string
	}{
		{name: "old prompt version", mutate: func(run *diagnosis.DiagnosisRun) { run.PromptVersion = "old-prompt" }, wantCode: ErrCodeExecutionSpecVersionUnsupported},
		{name: "old agent version", mutate: func(run *diagnosis.DiagnosisRun) { run.AgentVersion = "old-agent" }, wantCode: ErrCodeExecutionSpecVersionUnsupported},
		{name: "incompatible agent config hash", mutate: func(run *diagnosis.DiagnosisRun) { run.AgentConfigHash = "incompatible" }, wantCode: ErrCodeExecutionSpecConfigMismatch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := compatibleRuntimeDiagnosisRun("fc08-" + tc.name)
			tc.mutate(run)
			spec, err := diagnosis.BuildExecutionSpec(run)
			if err != nil {
				t.Fatal(err)
			}
			provider := &runtimeGenerationProvider{}
			factory := &runtimeProviderFactory{provider: provider}
			executor := NewAgentRuntimeExecutorWithFactory(factory, nil, nil, nil, DefaultGuardConfig())
			result, err := executor.Execute(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "validation-fc08-attempt"})
			class, code := jobs.ClassifyError(err)
			if class != jobs.ErrorClassPermanent || code != tc.wantCode {
				t.Fatalf("error = %v, want permanent %s", err, tc.wantCode)
			}
			if factory.builds != 0 || len(provider.requests) != 0 || result != nil {
				t.Fatalf("incompatible spec crossed provider boundary: factory builds=%d provider calls=%d result=%+v", factory.builds, len(provider.requests), result)
			}
		})
	}
}

func TestFC08CompatibleFrozenSpecReachesProviderWithJSONContract(t *testing.T) {
	provider := &runtimeGenerationProvider{}
	factory := &runtimeProviderFactory{provider: provider}
	executor := NewAgentRuntimeExecutorWithFactory(factory, nil, nil, nil, DefaultGuardConfig())
	spec, err := diagnosis.BuildExecutionSpec(compatibleRuntimeDiagnosisRun("fc08-current"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "fc08-current-attempt"})
	if err != nil {
		t.Fatalf("compatible current spec rejected: %v", err)
	}
	if factory.builds != 1 || len(provider.requests) != 1 || result == nil || result.ProviderCalls != 1 {
		t.Fatalf("compatible provider boundary: factory builds=%d requests=%d result=%+v", factory.builds, len(provider.requests), result)
	}
	if provider.requests[0].ResponseFormat == nil || provider.requests[0].ResponseFormat.Type != "json_object" {
		t.Fatalf("provider response format = %+v, want json_object", provider.requests[0].ResponseFormat)
	}
}

func TestFC08RealBenchNoneEffectiveFormatMatchesFrozenIdentity(t *testing.T) {
	run := compatibleRuntimeDiagnosisRun("fc08-realbench-none")
	run.AgentConfigHash = diagnosis.ComputeAgentConfigHashWithGenerationOptions(
		run.MaxAgentRounds, run.MaxToolCalls, run.MaxSearchCalls, run.MaxRepeatCalls,
		run.MaxEvidencePacketBytes, run.MaxToolResultBytes, run.FinalizationTurns,
		run.MaxOutputTokens, run.ProviderTimeoutSeconds, run.ProviderRetryAttempts,
		run.Temperature, run.ReasoningEffort, "none",
	)
	spec, err := diagnosis.BuildExecutionSpec(run)
	if err != nil {
		t.Fatal(err)
	}
	provider := &runtimeGenerationProvider{}
	factory := &runtimeProviderFactory{provider: provider}
	executor := NewAgentRuntimeExecutorWithFactory(factory, nil, nil, nil, DefaultGuardConfig()).
		WithGenerationOptions(GenerationOptions{ReasoningEffort: run.ReasoningEffort, ResponseFormat: nil})
	result, err := executor.Execute(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "fc08-realbench-none-attempt"})
	if err != nil {
		t.Fatalf("RealBench none execution rejected: %v", err)
	}
	if factory.builds != 1 || len(provider.requests) != 1 || result == nil || result.ProviderCalls != 1 {
		t.Fatalf("RealBench none provider boundary: factory builds=%d requests=%d result=%+v", factory.builds, len(provider.requests), result)
	}
	if provider.requests[0].ResponseFormat != nil {
		t.Fatalf("RealBench none request response_format = %+v, want nil", provider.requests[0].ResponseFormat)
	}
}

func TestFC08NonJSONRuntimeOverrideFailsBeforeProviderFactory(t *testing.T) {
	provider := &runtimeGenerationProvider{}
	factory := &runtimeProviderFactory{provider: provider}
	executor := NewAgentRuntimeExecutorWithFactory(factory, nil, nil, nil, DefaultGuardConfig()).WithGenerationOptions(GenerationOptions{})
	spec, err := diagnosis.BuildExecutionSpec(compatibleRuntimeDiagnosisRun("fc08-override"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "fc08-override-attempt"}); err == nil {
		t.Fatal("non-JSON runtime override was accepted")
	} else {
		class, code := jobs.ClassifyError(err)
		if class != jobs.ErrorClassPermanent || code != ErrCodeExecutionSpecConfigMismatch {
			t.Fatalf("override error = %v, want permanent %s", err, ErrCodeExecutionSpecConfigMismatch)
		}
	}
	if factory.builds != 0 || len(provider.requests) != 0 {
		t.Fatalf("non-JSON runtime override crossed provider boundary: factory builds=%d provider calls=%d", factory.builds, len(provider.requests))
	}
}

func compatibleRuntimeDiagnosisRun(id string) *diagnosis.DiagnosisRun {
	guard := DefaultGuardConfig()
	run := &diagnosis.DiagnosisRun{
		ID: id, IssueTitle: "FC-08 frozen execution identity",
		PromptVersion: diagnosis.CurrentPromptVersion, AgentVersion: diagnosis.CurrentAgentVersion,
		Temperature: 0.1, ReasoningEffort: "low",
		MaxAgentRounds: guard.MaxSteps, MaxToolCalls: guard.MaxToolCalls,
		MaxSearchCalls: guard.MaxSearchCalls, MaxRepeatCalls: guard.MaxRepeatCalls,
		MaxEvidencePacketBytes: 32 * 1024, MaxToolResultBytes: guard.MaxToolResultBytes,
		FinalizationTurns: 1, MaxOutputTokens: guard.MaxOutputTokens,
		ProviderTimeoutSeconds: 60,
	}
	run.AgentConfigHash = diagnosis.ComputeAgentConfigHashWithGenerationOptions(
		run.MaxAgentRounds, run.MaxToolCalls, run.MaxSearchCalls, run.MaxRepeatCalls,
		run.MaxEvidencePacketBytes, run.MaxToolResultBytes, run.FinalizationTurns,
		run.MaxOutputTokens, run.ProviderTimeoutSeconds, run.ProviderRetryAttempts,
		run.Temperature, run.ReasoningEffort, "json_object",
	)
	return run
}

type runtimeProviderFactory struct {
	provider llm.Provider
	builds   int
}

func (f *runtimeProviderFactory) BuildForExecution(context.Context, diagnosis.ProviderSnapshot) (llm.Provider, error) {
	f.builds++
	return f.provider, nil
}
