package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseReportJSONSkipsProseBracesBeforeFencedJSON(t *testing.T) {
	raw := "Analysis note: if shouldRedirect { shouldRedirect = false }\n\n" +
		"```json\n" +
		`{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[]}` +
		"\n```"

	report, err := parseReportJSON(raw)
	if err != nil {
		t.Fatalf("parseReportJSON failed: %v", err)
	}
	if report.RootCause != "root cause" {
		t.Fatalf("root cause = %q, want root cause", report.RootCause)
	}
}

func TestParseReportJSONAcceptsAttemptScopedEvidenceReference(t *testing.T) {
	report, err := parseReportJSON(`{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning","citations":[{"evidence_id":"ev_123","reason":"supports the finding"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := report.Findings[0].Citations[0].EvidenceID; got != "ev_123" {
		t.Fatalf("evidence ID = %q, want ev_123", got)
	}
}

func TestBoundToolResultDoesNotSplitEvidenceJSON(t *testing.T) {
	result := boundToolResult("read_file", strings.Repeat("source", 20), 16)
	if len(result) > 16 {
		// A structured error is deliberately short but may still exceed an
		// unusually tiny caller limit; it must never contain a partial source.
		if strings.Contains(result, "source") {
			t.Fatalf("oversized result leaked partial source: %q", result)
		}
	}
	if !json.Valid([]byte(result)) {
		t.Fatalf("evidence overflow result is not valid JSON: %q", result)
	}
}

func TestSystemPromptUsesEvidenceIDOnlyForFinalCitations(t *testing.T) {
	for _, field := range []string{`"path"`, `"start_line"`, `"end_line"`, `"excerpt"`, `"content_hash"`} {
		if strings.Contains(SystemPrompt, field) {
			t.Fatalf("system prompt still requests legacy citation field %s", field)
		}
	}
	if !strings.Contains(SystemPrompt, `"evidence_id"`) {
		t.Fatal("system prompt does not define evidence_id citation field")
	}
}
