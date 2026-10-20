package codeintel_test

import (
	"context"
	"testing"

	"repolens/internal/codeintel"
	m "repolens/internal/codeintel/model"
	"repolens/internal/retrieval"
)

func TestInitializerLocalTypesDoNotEnterPersistedCodeIndex(t *testing.T) {
	result, fs := analyzeCorrectnessFixture(t, map[string]string{
		"main.go": "package correctness\ntype Payload struct{ Value int }\nfunc Work() {}\n",
		"spec_test.go": `package correctness_test
import core "example.com/correctness"
type ExportedAlias = core.Payload
func register(body func()) bool { return true }
var _ = register(func() {
 type Payload struct{ Local int }
 type LocalAlias = int
 type LocalInterface interface{ M() }
 _ = register(func() { type Payload struct{ Nested string }; core.Work() })
})
var _ = register(func() {
 type Payload struct{ Other string }
 type LocalAlias = string
 type LocalInterface interface{ N() }
})
func TestWork() { core.Work() }
`,
	})
	want := map[string]bool{
		"example.com/correctness|example.com/correctness||TYPE|Payload":            true,
		"example.com/correctness|example.com/correctness||FUNCTION|Work":           true,
		"example.com/correctness|example.com/correctness_test||TYPE|ExportedAlias": true,
		"example.com/correctness|example.com/correctness_test||FUNCTION|register":  true,
		"example.com/correctness|example.com/correctness_test||FUNCTION|TestWork":  true,
	}
	if len(result.Symbols) != len(want) {
		t.Fatalf("got %d symbols, want %d", len(result.Symbols), len(want))
	}
	seen := map[string]bool{}
	for _, sym := range result.Symbols {
		if !want[sym.SymbolKeyRaw] || seen[sym.SymbolKeyRaw] {
			t.Fatalf("unexpected or duplicate symbol: %+v", sym)
		}
		seen[sym.SymbolKeyRaw] = true
	}
	_, _, store, _ := setupCodeIntelTestDB(t)
	ctx := context.Background()
	build, _, err := store.GetOrCreateBuild(ctx, "snap", result.ModulePath, codeintel.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkBuildBuilding(ctx, build.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveAnalysisResult(ctx, build.ID, result); err != nil {
		t.Fatal(err)
	}
	ready, err := store.GetByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != m.BuildStatusReady || ready.SymbolCount != len(want) {
		t.Fatalf("incomplete CodeIndex: %+v", ready)
	}
	persisted, err := store.ListAllSymbols(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted) != len(want) {
		t.Fatalf("persisted %d symbols, want %d", len(persisted), len(want))
	}
	idx, err := retrieval.BuildSymbolIndex(ctx, persisted, &retrieval.SymbolIndexSource{Store: fs, RepositoryID: "repo", SnapshotID: "snap"})
	if err != nil {
		t.Fatal(err)
	}
	if idx.TotalDocs != len(want) {
		t.Fatalf("BM25 documents=%d, want %d", idx.TotalDocs, len(want))
	}
	for _, doc := range idx.Documents {
		if doc.SymbolName == "LocalAlias" || doc.SymbolName == "LocalInterface" || (doc.SymbolName == "Payload" && doc.FilePath != "main.go") {
			t.Fatalf("local type leaked into BM25: %+v", doc)
		}
	}
}

func TestPackageScopeAnalyzerDoesNotReusePreviousReadyBuild(t *testing.T) {
	db, _, store, _ := setupCodeIntelTestDB(t)
	ctx := context.Background()
	bc := codeintel.DefaultBuildContext()
	old := &m.CodeIndexBuild{SnapshotID: "scope-version", ModulePath: "example.com/scope", ParserVersion: m.CurrentParserVersion, AnalyzerVersion: "v2.2.3", SymbolSchemaVersion: m.CurrentSymbolSchemaVersion, BuildContextHash: bc.BuildContextHash(), Status: m.BuildStatusReady}
	if err := db.Create(old).Error; err != nil {
		t.Fatal(err)
	}
	current, created, err := store.GetOrCreateBuild(ctx, old.SnapshotID, old.ModulePath, bc)
	if err != nil || !created || current.ID == old.ID || current.AnalyzerVersion != m.CurrentAnalyzerVersion {
		t.Fatalf("previous analyzer build reused: %+v created=%t err=%v", current, created, err)
	}
	again, created, err := store.GetOrCreateBuild(ctx, old.SnapshotID, old.ModulePath, bc)
	if err != nil || created || again.ID != current.ID {
		t.Fatalf("new build identity is not idempotent: %+v created=%t err=%v", again, created, err)
	}
	historical, err := store.GetByID(ctx, old.ID)
	if err != nil || historical.Status != m.BuildStatusReady || historical.AnalyzerVersion != "v2.2.3" {
		t.Fatalf("historical READY build changed: %+v %v", historical, err)
	}
	oldRetrieval, _, err := store.GetOrCreateRetrievalBuild(ctx, old.ID, m.ProductionRetrievalStrategy)
	if err != nil {
		t.Fatal(err)
	}
	newRetrieval, created, err := store.GetOrCreateRetrievalBuild(ctx, current.ID, m.ProductionRetrievalStrategy)
	if err != nil || !created || oldRetrieval.ID == newRetrieval.ID || newRetrieval.CodeIndexBuildID != current.ID {
		t.Fatalf("retrieval build reused old lineage: %+v %v", newRetrieval, err)
	}
}
