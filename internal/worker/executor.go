package worker

import (
	"context"

	"repolens/internal/agent"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
)

type ExecutionResult = agent.ExecutionResult
type DiagnosisExecutor = agent.Executor

type FakeDiagnosisExecutor struct{}

func NewFakeDiagnosisExecutor() *FakeDiagnosisExecutor {
	return &FakeDiagnosisExecutor{}
}

func (e *FakeDiagnosisExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*ExecutionResult, error) {
	report := &evidence.DiagnosisReportData{
		Summary:   "Root cause identified in repository repository based on logs and code structure",
		RootCause: "Null pointer dereference in handler initialization logic",
		Findings: []evidence.Finding{
			{
				Title:     "Missing nil check during config loading",
				Reasoning: "The database configuration pointer was accessed without verifying if the config struct was populated",
				Citations: []evidence.Citation{
					{
						SnapshotID: run.SnapshotID,
						FilePath:   "internal/platform/config/config.go",
						StartLine:  1,
						EndLine:    15,
						Excerpt:    "package config",
						Reason:     "Config package entrypoint",
					},
				},
			},
		},
		RecommendedChecks: []string{
			"Add explicit nil check before accessing DB config",
			"Add regression test case in unit tests",
		},
		Confidence: 0.95,
	}

	return &ExecutionResult{
		Report:           report,
		PromptTokens:     150,
		CompletionTokens: 300,
		ToolCalls:        2,
		Retryable:        false,
	}, nil
}
