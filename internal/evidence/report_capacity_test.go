package evidence

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReportSerializedByteContract(t *testing.T) {
	data := &DiagnosisReportData{ConclusionKind: ConclusionRootCause, Summary: "summary", RootCause: "cause"}
	for i := 0; i < 8; i++ {
		finding := Finding{Title: "title", Reasoning: "reason"}
		for j := 0; j < 16; j++ {
			finding.Citations = append(finding.Citations, Citation{FilePath: "foo.go", Excerpt: strings.Repeat("x", 32768), ValidationStatus: CitationValid})
		}
		data.Findings = append(data.Findings, finding)
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	excess := len(encoded) - MaxReportBytes
	last := &data.Findings[7].Citations[15]
	if excess <= 0 || excess >= len(last.Excerpt) {
		t.Fatalf("unexpected fixture excess=%d", excess)
	}
	last.Excerpt = last.Excerpt[:len(last.Excerpt)-excess]
	encoded, _ = json.Marshal(data)
	if len(encoded) != MaxReportBytes {
		t.Fatalf("serialized boundary=%d", len(encoded))
	}
	if err := ValidateReportStructure(data); err != nil {
		t.Fatalf("legal boundary: %v", err)
	}
	last.Excerpt += "x"
	if err := ValidateReportStructure(data); !errors.Is(err, ErrReportTooLarge) {
		t.Fatalf("over-boundary error=%v", err)
	}
	// JSON escaping matters: raw string length alone is not the byte contract.
	last.Excerpt = strings.Repeat("\x00", 32768)
	if err := ValidateReportStructure(data); !errors.Is(err, ErrReportTooLarge) {
		t.Fatalf("escaped report error=%v", err)
	}
}

func TestReportPersistenceFieldBoundaries(t *testing.T) {
	for _, field := range []string{"findings", "checks", "limitations", "structured", "raw", "summary", "cause", "parse"} {
		t.Run(field, func(t *testing.T) {
			rep := &Report{}
			limit := MaxReportBytes
			var target *string
			switch field {
			case "findings":
				target = &rep.FindingsJSON
			case "checks":
				target = &rep.RecommendedChecksJSON
			case "limitations":
				target = &rep.LimitationsJSON
			case "structured":
				target = &rep.StructuredPayloadJSON
			case "raw":
				target = &rep.RawOutput
			case "summary":
				target = &rep.Summary
				limit = 65535
			case "cause":
				target = &rep.RootCause
				limit = 65535
			case "parse":
				target = &rep.ParseError
				limit = 65535
			}
			*target = strings.Repeat("x", limit)
			if err := ValidateReportPersistence(rep); err != nil {
				t.Fatal(err)
			}
			*target += "x"
			if err := ValidateReportPersistence(rep); !errors.Is(err, ErrReportTooLarge) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}
