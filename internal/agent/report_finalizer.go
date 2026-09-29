package agent

import (
	"context"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
)

func FinalizeReport(ctx context.Context, issuer evidence.EvidenceIssuer, draft *evidence.ReportDraft, spec diagnosis.DiagnosisExecutionSpec, attempt *diagnosis.DiagnosisAttempt) *evidence.DiagnosisReportData {
	if draft == nil {
		return &evidence.DiagnosisReportData{}
	}
	report, _ := evidence.ResolveReportDraft(ctx, issuer, draft, evidence.DraftLineage{
		AttemptID:        attempt.ID,
		DiagnosisRunID:   spec.RunID,
		RepositoryID:     spec.Lineage.RepositoryID,
		SnapshotID:       spec.Lineage.SnapshotID,
		CodeIndexBuildID: spec.Lineage.CodeIndexBuildID,
	})
	if report == nil {
		return &evidence.DiagnosisReportData{}
	}
	return report
}
