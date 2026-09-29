package agent

import (
	"context"
	"testing"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
)

type finalizerIssuer struct{}

func (finalizerIssuer) Issue(context.Context, evidence.IssueRequest) (*evidence.AttemptEvidenceItem, error) {
	return nil, evidence.ErrEvidenceSourceUnavailable
}

func (finalizerIssuer) Resolve(_ context.Context, attemptID, evidenceID string) (*evidence.AttemptEvidenceItem, error) {
	if evidenceID != "ev_valid" {
		return nil, evidence.ErrEvidenceNotFound
	}
	return &evidence.AttemptEvidenceItem{
		ID: evidenceID, AttemptID: attemptID, DiagnosisRunID: "run", SnapshotID: "snapshot", CodeIndexBuildID: 17,
		FilePath: "main.go", StartLine: 1, EndLine: 2, DisplayExcerpt: "package main", RawContentHash: "hash",
	}, nil
}

func TestFinalizeReportPreservesAttemptScopedCitationValidation(t *testing.T) {
	spec := diagnosis.DiagnosisExecutionSpec{RunID: "run"}
	spec.Lineage.RepositoryID = "repo"
	spec.Lineage.SnapshotID = "snapshot"
	spec.Lineage.CodeIndexBuildID = 17
	attempt := &diagnosis.DiagnosisAttempt{ID: "attempt"}
	draft := &evidence.ReportDraft{
		ConclusionKind: evidence.ConclusionRootCause, Summary: "summary", RootCause: "cause",
		Findings: []evidence.FindingDraft{{Title: "finding", Reasoning: "reasoning", Citations: []evidence.CitationRef{
			{EvidenceID: "ev_valid", Reason: "valid support"},
			{EvidenceID: "ev_missing", Reason: "missing support"},
		}}},
	}
	report := FinalizeReport(context.Background(), finalizerIssuer{}, draft, spec, attempt)
	if report == nil || len(report.Findings) != 1 || len(report.Findings[0].Citations) != 2 {
		t.Fatalf("finalized report = %+v", report)
	}
	valid, missing := report.Findings[0].Citations[0], report.Findings[0].Citations[1]
	if valid.ValidationStatus != evidence.CitationValid || valid.FilePath != "main.go" || valid.SnapshotID != "snapshot" || valid.CodeIndexBuildID != 17 {
		t.Fatalf("valid citation = %+v", valid)
	}
	if missing.ValidationStatus != evidence.CitationInvalid || missing.ValidationError != "EVIDENCE_NOT_FOUND" {
		t.Fatalf("missing citation = %+v", missing)
	}
	if empty := FinalizeReport(context.Background(), finalizerIssuer{}, nil, spec, attempt); empty == nil || len(empty.Findings) != 0 {
		t.Fatalf("nil draft finalization = %+v", empty)
	}
}
