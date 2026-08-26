package codeintel_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/codeintel"
)

func TestCanonicalizeReceiver(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"*Service", "Service"},
		{"Service", "Service"},
		{"(s *Service)", "Service"},
		{"(s Service)", "Service"},
		{"s *Service", "Service"},
		{"s Service", "Service"},
		{"(*Calculator)", "Calculator"},
		{"*pkg.Type", "pkg.Type"},
		{"*Stack[T]", "Stack"},
		{"Stack[T, R]", "Stack"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := codeintel.CanonicalizeReceiver(tt.input)
			if got != tt.expected {
				t.Errorf("CanonicalizeReceiver(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestSymbolKeyGeneration(t *testing.T) {
	raw, hash := codeintel.BuildSymbolKey("example.com/mod", "pkg/auth", "Service", codeintel.SymbolKindMethod, "Authenticate")
	expectedRaw := "example.com/mod|pkg/auth|Service|METHOD|Authenticate"
	if raw != expectedRaw {
		t.Errorf("raw key = %q, want %q", raw, expectedRaw)
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}

	// Determinism check
	raw2, hash2 := codeintel.BuildSymbolKey("example.com/mod", "pkg/auth", "Service", codeintel.SymbolKindMethod, "Authenticate")
	if raw != raw2 || hash != hash2 {
		t.Errorf("SymbolKey generation is not deterministic")
	}
}

func TestBuildContextHash(t *testing.T) {
	bctx1 := codeintel.DefaultBuildContext()
	bctx2 := codeintel.DefaultBuildContext()
	if bctx1.BuildContextHash() != bctx2.BuildContextHash() {
		t.Errorf("BuildContextHash should be deterministic")
	}

	bctx3 := codeintel.BuildContext{GOOS: "windows", GOARCH: "amd64", BuildTags: []string{"pro"}}
	if bctx1.BuildContextHash() == bctx3.BuildContextHash() {
		t.Errorf("Different BuildContext should produce different hash")
	}
}

func TestAnalyze_ParserFixtures(t *testing.T) {
	fixtureDir, err := filepath.Abs("../../testdata/parser_fixtures")
	if err != nil {
		t.Fatalf("failed to resolve fixture path: %v", err)
	}

	analyzer := codeintel.NewAnalyzer()
	ctx := context.Background()
	bctx := codeintel.DefaultBuildContext()

	result, err := analyzer.Analyze(ctx, fixtureDir, bctx)
	if err != nil {
		t.Fatalf("Analyze failed: %v", err)
	}

	// 1. Module info
	if result.ModulePath != "example.com/fixture" {
		t.Errorf("expected module path example.com/fixture, got %q", result.ModulePath)
	}

	// 2. Build Context filtering
	var foundLinuxFile, foundWindowsFile bool
	for _, f := range result.Files {
		if strings.Contains(f.Path, "build_tags_linux.go") {
			foundLinuxFile = true
			if !f.IncludedByBuildContext || f.ParseStatus != "OK" {
				t.Errorf("build_tags_linux.go should be included and parsed OK under linux context, got included=%v, status=%s", f.IncludedByBuildContext, f.ParseStatus)
			}
		}
		if strings.Contains(f.Path, "build_tags_windows.go") {
			foundWindowsFile = true
			if f.IncludedByBuildContext || f.ParseStatus != "SKIPPED" {
				t.Errorf("build_tags_windows.go should be SKIPPED under linux context, got included=%v, status=%s", f.IncludedByBuildContext, f.ParseStatus)
			}
		}
	}
	if !foundLinuxFile || !foundWindowsFile {
		t.Errorf("expected to find build_tags_linux.go and build_tags_windows.go")
	}

	// 3. Syntax error file handling
	var foundSyntaxErrorFile bool
	for _, f := range result.Files {
		if strings.Contains(f.Path, "syntax_error.go") {
			foundSyntaxErrorFile = true
			if f.ParseStatus != "ERROR" {
				t.Errorf("syntax_error.go should have ParseStatus=ERROR, got %s", f.ParseStatus)
			}
		}
	}
	if !foundSyntaxErrorFile {
		t.Errorf("expected to find syntax_error.go")
	}

	// 4. Nested module warning
	var foundNestedWarning bool
	for _, w := range result.Quality.Warnings {
		if strings.Contains(w, "nested go.mod") {
			foundNestedWarning = true
			break
		}
	}
	if !foundNestedWarning {
		t.Errorf("expected warning about nested go.mod, got warnings: %v", result.Quality.Warnings)
	}

	// 5. Symbol extraction checks
	symbolNames := make(map[string]*codeintel.Symbol)
	for _, sym := range result.Symbols {
		symbolNames[sym.QualifiedName] = sym
	}

	expectedSymbols := []struct {
		qualName string
		kind     codeintel.SymbolKind
		exported bool
	}{
		{"fixture.Add", codeintel.SymbolKindFunction, true},
		{"fixture.Divide", codeintel.SymbolKindFunction, true},
		{"fixture.Calculator.Compute", codeintel.SymbolKindMethod, true},
		{"fixture.Calculator.Reset", codeintel.SymbolKindMethod, true},
		{"fixture.Service.Execute", codeintel.SymbolKindMethod, true},
		{"fixture.Calculator", codeintel.SymbolKindType, true},
		{"fixture.Reader", codeintel.SymbolKindInterface, true},
		{"fixture.ReadCloser", codeintel.SymbolKindInterface, true},
		{"fixture.Stack", codeintel.SymbolKindType, true},
		{"fixture.Stack.Push", codeintel.SymbolKindMethod, true},
		{"fixture.Account", codeintel.SymbolKindType, true},
		{"pkg_a.Worker.Process", codeintel.SymbolKindMethod, true},
		{"pkg_b.Worker.Process", codeintel.SymbolKindMethod, true},
		{"pkg_a.GlobalInit", codeintel.SymbolKindFunction, true},
		{"pkg_b.GlobalInit", codeintel.SymbolKindFunction, true},
	}

	for _, es := range expectedSymbols {
		sym, exists := symbolNames[es.qualName]
		if !exists {
			t.Errorf("missing expected symbol: %s", es.qualName)
			continue
		}
		if sym.Kind != es.kind {
			t.Errorf("symbol %s: expected kind %s, got %s", es.qualName, es.kind, sym.Kind)
		}
		if sym.Exported != es.exported {
			t.Errorf("symbol %s: expected exported %v, got %v", es.qualName, es.exported, sym.Exported)
		}
		if sym.StartLine <= 0 || sym.EndLine < sym.StartLine {
			t.Errorf("symbol %s has invalid line range: %d-%d", es.qualName, sym.StartLine, sym.EndLine)
		}
		if sym.ContentHash == "" {
			t.Errorf("symbol %s has empty ContentHash", es.qualName)
		}
	}

	// 6. Distinct symbols for same-name across packages
	symPkgA := symbolNames["pkg_a.Worker.Process"]
	symPkgB := symbolNames["pkg_b.Worker.Process"]
	if symPkgA != nil && symPkgB != nil {
		if symPkgA.SymbolKeyHash == symPkgB.SymbolKeyHash {
			t.Errorf("pkg_a and pkg_b same-name symbols must have distinct SymbolKeyHash")
		}
	}

	// 7. Relations check (semantic call, syntactic call, unresolved external)
	var foundSemanticCall, foundSyntacticCall, foundUnresolvedCall bool
	for _, r := range result.Relations {
		if r.ResolutionKind == codeintel.ResolutionKindSemantic {
			foundSemanticCall = true
		}
		if r.ResolutionKind == codeintel.ResolutionKindSyntactic {
			foundSyntacticCall = true
		}
		if r.ResolutionKind == codeintel.ResolutionKindUnresolved {
			foundUnresolvedCall = true
			if r.TargetPackagePath != "github.com/external/missing" {
				t.Errorf("unresolved relation expected package github.com/external/missing, got %s", r.TargetPackagePath)
			}
		}
	}

	if !foundSemanticCall {
		t.Errorf("expected to find at least one SEMANTIC relation")
	}
	if !foundSyntacticCall {
		t.Logf("note: no purely syntactic calls detected")
	}
	if !foundUnresolvedCall {
		t.Errorf("expected to find at least one UNRESOLVED relation from unresolved_import.go")
	}

	// 8. Related Tests Discovery
	if len(result.RelatedTests) == 0 {
		t.Errorf("expected related tests to be discovered, got 0")
	}

	reasonsFound := make(map[codeintel.TestRelationReason]bool)
	for _, rt := range result.RelatedTests {
		reasonsFound[rt.ReasonCode] = true
	}

	if !reasonsFound[codeintel.TestReasonDirectSemantic] && !reasonsFound[codeintel.TestReasonDirectSyntactic] {
		t.Errorf("expected direct test usage signals, got %v", reasonsFound)
	}
	if !reasonsFound[codeintel.TestReasonNameMatch] {
		t.Errorf("expected NAME_MATCH signal, got %v", reasonsFound)
	}

	// 9. AnalysisQuality metrics
	q := result.Quality
	if q.FilesTotal == 0 || q.FilesParsed == 0 {
		t.Errorf("invalid files quality count: total=%d, parsed=%d", q.FilesTotal, q.FilesParsed)
	}
	if q.SymbolsTotal == 0 {
		t.Errorf("invalid symbols quality count: total=%d", q.SymbolsTotal)
	}
	if q.SemanticRelationsCount == 0 && q.SyntacticRelationsCount == 0 {
		t.Errorf("expected relations in quality count: semantic=%d, syntactic=%d", q.SemanticRelationsCount, q.SyntacticRelationsCount)
	}
}
