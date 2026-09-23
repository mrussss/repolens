package evidence

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidateReportDraftNormalizesCitationReasonBeforeStructure(t *testing.T) {
	draft := &ReportDraft{
		ConclusionKind: ConclusionRootCause, Summary: "summary", RootCause: "cause",
		Findings: []FindingDraft{{Title: "finding", Reasoning: "reasoning", Citations: []CitationRef{{
			EvidenceID: "ev_1", Reason: strings.Repeat("界", 1000), LegacyPath: strings.Repeat("x", 400), LegacyExcerpt: strings.Repeat("y", 40*1024),
		}}}},
	}
	if err := ValidateReportDraftStructure(draft); err != nil {
		t.Fatalf("normalized citation draft rejected: %v", err)
	}
	normalized := limitCitationReason(draft.Findings[0].Citations[0].Reason)
	if len(normalized) > 2048 || !utf8.ValidString(normalized) {
		t.Fatalf("citation reason normalization is not UTF-8 safe: bytes=%d valid=%t", len(normalized), utf8.ValidString(normalized))
	}
}
