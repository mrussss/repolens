package evidence_test

import (
	"strings"
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
		Summary:   "summary",
		RootCause: "root cause",
		Findings:  []evidence.Finding{{Title: "finding", Reasoning: "reasoning"}},
	}, true)
	if err == nil || quality.Status != evidence.ReportInvalid {
		t.Fatalf("missing conclusion kind quality = %+v err=%v", quality, err)
	}

	quality, err = evidence.ClassifyReport(&evidence.DiagnosisReportData{
		ConclusionKind:    evidence.ConclusionInsufficientEvidence,
		ConfirmedFacts:    []string{"the available evidence is incomplete"},
		Limitations:       []string{"not enough evidence"},
		RecommendedChecks: []string{"collect more evidence"},
	}, true)
	if err != nil || quality.Status != evidence.ReportInsufficientEvidence {
		t.Fatalf("insufficient quality = %+v err=%v", quality, err)
	}
}

func TestValidateReportStructureUsesOneCompleteContract(t *testing.T) {
	base := func() *evidence.DiagnosisReportData {
		return &evidence.DiagnosisReportData{
			ConclusionKind: evidence.ConclusionRootCause,
			Summary:        "summary",
			RootCause:      "root cause",
			Findings:       []evidence.Finding{{Title: "finding", Reasoning: "reasoning"}},
		}
	}
	tests := []struct {
		name   string
		mutate func(*evidence.DiagnosisReportData)
	}{
		{name: "confidence outside range", mutate: func(report *evidence.DiagnosisReportData) { value := 2.0; report.ModelClaimedConfidence = &value }},
		{name: "long summary", mutate: func(report *evidence.DiagnosisReportData) { report.Summary = strings.Repeat("s", 16*1024+1) }},
		{name: "long list value", mutate: func(report *evidence.DiagnosisReportData) {
			report.RecommendedChecks = []string{strings.Repeat("c", 8*1024+1)}
		}},
		{name: "too many findings", mutate: func(report *evidence.DiagnosisReportData) {
			report.Findings = make([]evidence.Finding, 65)
			for i := range report.Findings {
				report.Findings[i] = evidence.Finding{Title: "finding", Reasoning: "reasoning"}
			}
		}},
		{name: "too many recommended checks", mutate: func(report *evidence.DiagnosisReportData) { report.RecommendedChecks = make([]string, 33) }},
		{name: "too many confirmed facts", mutate: func(report *evidence.DiagnosisReportData) { report.ConfirmedFacts = make([]string, 33) }},
		{name: "too many limitations", mutate: func(report *evidence.DiagnosisReportData) { report.Limitations = make([]string, 33) }},
		{name: "too many citations", mutate: func(report *evidence.DiagnosisReportData) {
			report.Findings[0].Citations = make([]evidence.Citation, 17)
		}},
		{name: "empty finding title", mutate: func(report *evidence.DiagnosisReportData) { report.Findings[0].Title = "   " }},
		{name: "empty finding reasoning", mutate: func(report *evidence.DiagnosisReportData) { report.Findings[0].Reasoning = "" }},
		{name: "long finding title", mutate: func(report *evidence.DiagnosisReportData) { report.Findings[0].Title = strings.Repeat("t", 2*1024+1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := base()
			tt.mutate(report)
			if err := evidence.ValidateReportStructure(report); err == nil {
				t.Fatal("expected structural validation failure")
			}
			quality, err := evidence.ClassifyReport(report, true)
			if err == nil || quality.Status != evidence.ReportInvalid {
				t.Fatalf("classification = %+v err=%v, want INVALID", quality, err)
			}
		})
	}
}
