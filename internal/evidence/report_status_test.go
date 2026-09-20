package evidence_test

import (
	"testing"

	"repolens/internal/evidence"
)

func TestClassifyReportSeparatesExecutionShapeFromEvidenceQuality(t *testing.T) {
	valid := &evidence.DiagnosisReportData{
		ConclusionKind: evidence.ConclusionRootCause,
		Summary:        "summary",
		RootCause:      "root cause",
		Findings: []evidence.Finding{{
			Title: "finding", Reasoning: "reasoning",
			Citations: []evidence.Citation{{ValidationStatus: evidence.CitationValid}},
		}},
	}
	quality, err := evidence.ClassifyReport(valid, true)
	if err != nil || quality.Status != evidence.ReportValid || quality.CitationCoverage == nil || *quality.CitationCoverage != 1 {
		t.Fatalf("valid quality = %+v err=%v", quality, err)
	}

	degraded := *valid
	degraded.Findings = []evidence.Finding{{Title: "unsupported", Reasoning: "reasoning"}}
	quality, err = evidence.ClassifyReport(&degraded, true)
	if err != nil || quality.Status != evidence.ReportDegraded || quality.CitationCoverage == nil || *quality.CitationCoverage != 0 {
		t.Fatalf("degraded quality = %+v err=%v", quality, err)
	}

	quality, err = evidence.ClassifyReport(&evidence.DiagnosisReportData{}, false)
	if err == nil || quality.Status != evidence.ReportInvalid {
		t.Fatalf("invalid quality = %+v err=%v", quality, err)
	}

	quality, err = evidence.ClassifyReport(&evidence.DiagnosisReportData{
		ConclusionKind: evidence.ConclusionInsufficientEvidence,
		Limitations:    []string{"not enough evidence"},
	}, true)
	if err != nil || quality.Status != evidence.ReportInsufficientEvidence {
		t.Fatalf("insufficient quality = %+v err=%v", quality, err)
	}
}
