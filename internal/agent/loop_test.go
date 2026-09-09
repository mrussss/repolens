package agent

import "testing"

func TestParseReportJSONSkipsProseBracesBeforeFencedJSON(t *testing.T) {
	raw := "Analysis note: if shouldRedirect { shouldRedirect = false }\n\n" +
		"```json\n" +
		`{"summary":"summary","root_cause":"root cause","findings":[]}` +
		"\n```"

	report, err := parseReportJSON(raw)
	if err != nil {
		t.Fatalf("parseReportJSON failed: %v", err)
	}
	if report.RootCause != "root cause" {
		t.Fatalf("root cause = %q, want root cause", report.RootCause)
	}
}
