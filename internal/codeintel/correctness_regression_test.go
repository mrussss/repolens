package codeintel_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/codeintel"
	m "repolens/internal/codeintel/model"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
)

func analyzeCorrectnessFixture(t *testing.T, files map[string]string) (*m.AnalysisResult, *snapshotstore.LocalSnapshotStore) {
	t.Helper()
	fs := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := fs.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	files["go.mod"] = "module example.com/correctness\n\ngo 1.22\n"
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := codeintel.NewAnalyzer().Analyze(context.Background(), root, codeintel.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if result.Quality.PackagesFailed != 0 || result.Quality.PackagesTypechecked == 0 {
		t.Fatalf("fixture must typecheck: %+v", result.Quality)
	}
	return result, fs
}

func TestLineDirectivesUsePhysicalSnapshotCoordinates(t *testing.T) {
	result, fs := analyzeCorrectnessFixture(t, map[string]string{
		"main.go":      "package correctness\n\n//line original.go:1000\nfunc Run() { marker := 1; _ = marker }\ntype A struct{}\nfunc (A) Do() {}\nfunc Caller(a A) { Run(); a.Do() }\n",
		"main_test.go": "package correctness\n//line tests.go:2000\nfunc TestRun() { Run() }\n",
	})
	byName := map[string]*m.Symbol{}
	for _, sym := range result.Symbols {
		byName[sym.Name] = sym
	}
	for name, line := range map[string]int{"Run": 4, "A": 5, "Do": 6, "Caller": 7, "TestRun": 3} {
		sym := byName[name]
		col := 1
		if name == "A" {
			col = 6
		}
		if sym == nil || sym.StartLine != line || sym.EndLine != line || sym.StartCol != col {
			t.Fatalf("%s physical range: %+v", name, sym)
		}
	}
	run := byName["Run"]
	want := "func Run() { marker := 1; _ = marker }"
	if run.FilePath != "main.go" || run.SourceExcerpt != want || run.ContentHash != fmt.Sprintf("%x", sha256.Sum256([]byte(want))) {
		t.Fatalf("wrong snapshot declaration: %+v", run)
	}
	calls, refs := 0, 0
	for _, rel := range result.Relations {
		if rel.FromSymbolKeyHash != byName["Caller"].SymbolKeyHash {
			continue
		}
		if rel.FilePath != "main.go" || rel.Line != 7 || rel.Column < 19 {
			t.Fatalf("wrong physical relation: %+v", rel)
		}
		if rel.RelationType == m.RelationTypeCallCandidate {
			calls++
		}
		if rel.RelationType == m.RelationTypeReference {
			refs++
		}
	}
	if calls != 2 || refs != 1 {
		t.Fatalf("caller matching lost: calls=%d refs=%d", calls, refs)
	}
	foundTest := false
	for _, d := range result.RelatedTests {
		if d.TargetSymbolKeyHash == run.SymbolKeyHash && d.TestSymbolKeyHash == byName["TestRun"].SymbolKeyHash {
			if d.TestLine != 3 || d.TestFilePath != "main_test.go" {
				t.Fatalf("logical test coordinate: %+v", d)
			}
			foundTest = true
		}
	}
	if !foundTest {
		t.Fatal("related test not discovered")
	}
	// Use persisted symbols, whose SourceExcerpt is not a database column.
	_, _, store, _ := setupCodeIntelTestDB(t)
	build, _, err := store.GetOrCreateBuild(context.Background(), "snap", result.ModulePath, codeintel.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkBuildBuilding(context.Background(), build.ID); err != nil {
		t.Fatal(err)
	}
	if err = store.SaveAnalysisResult(context.Background(), build.ID, result); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.ListAllSymbols(context.Background(), build.ID)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := retrieval.BuildSymbolIndex(context.Background(), persisted, &retrieval.SymbolIndexSource{Store: fs, RepositoryID: "repo", SnapshotID: "snap"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, doc := range idx.Documents {
		if doc.SymbolKeyHash == run.SymbolKeyHash {
			found = true
			if doc.FilePath != "main.go" || doc.StartLine != 4 || !strings.Contains(doc.Content, want) {
				t.Fatalf("incorrect BM25 source: %+v", doc)
			}
		}
	}
	if !found {
		t.Fatal("Run document absent")
	}
}

func TestMethodReferencesNeverTargetSameNamedFunction(t *testing.T) {
	for _, tc := range []struct {
		name, declaration, caller string
		wantCall                  bool
	}{
		{"value", "func (A) Run() {}", "func Caller(a A) { a.Run() }", true},
		{"pointer", "func (*A) Run() {}", "func Caller(a *A) { a.Run() }", true},
		{"method_value", "func (A) Run() {}", "func Caller(a A) { f := a.Run; f() }", false},
		{"method_expression", "func (A) Run() {}", "func Caller(a A) { A.Run(a) }", true},
		{"promoted", "func (A) Run() {}\ntype B struct{ A }", "func Caller(b B) { b.Run() }", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, _ := analyzeCorrectnessFixture(t, map[string]string{"main.go": "package correctness\nfunc Run() {}\ntype A struct{}\n" + tc.declaration + "\n" + tc.caller + "\n"})
			var caller, method, function *m.Symbol
			for _, sym := range result.Symbols {
				if sym.Name == "Caller" {
					caller = sym
				}
				if sym.Name == "Run" && sym.Kind == m.SymbolKindMethod {
					method = sym
				}
				if sym.Name == "Run" && sym.Kind == m.SymbolKindFunction {
					function = sym
				}
			}
			if caller == nil || method == nil || function == nil {
				t.Fatal("fixture symbols missing")
			}
			reference, call := false, false
			for _, rel := range result.Relations {
				if rel.FromSymbolKeyHash != caller.SymbolKeyHash {
					continue
				}
				if rel.ToSymbolKeyHash == function.SymbolKeyHash {
					t.Fatalf("false package-level reference/call: %+v", rel)
				}
				if rel.ToSymbolKeyHash == method.SymbolKeyHash && rel.ResolutionKind == m.ResolutionKindSemantic && rel.Confidence == 1 {
					reference = reference || rel.RelationType == m.RelationTypeReference
					call = call || rel.RelationType == m.RelationTypeCallCandidate
				}
			}
			if !reference || (tc.wantCall && !call) {
				t.Fatalf("method relation missing: reference=%v call=%v", reference, call)
			}
		})
	}
}

func TestInitDeclarationsPersistDistinctCallerGraphs(t *testing.T) {
	for _, layout := range []string{"same_line", "separate_lines", "separate_files"} {
		t.Run(layout, func(t *testing.T) {
			files := map[string]string{"main.go": "package correctness\nfunc A() {}\nfunc B() {}\n//line logical.go:1000\nfunc init() { A() }; func init() { B() }\n"}
			if layout == "separate_lines" {
				files["main.go"] = "package correctness\nfunc A() {}\nfunc B() {}\n//line logical.go:1000\nfunc init() { A() }\nfunc init() { B() }\n"
			}
			if layout == "separate_files" {
				files["main.go"] = "package correctness\nfunc A() {}\nfunc B() {}\n//line logical.go:1000\nfunc init() { A() }\n"
				files["other.go"] = "package correctness\n//line logical.go:1000\nfunc init() { B() }\n"
			}
			result, _ := analyzeCorrectnessFixture(t, files)
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
			symbols, err := store.ListAllSymbols(ctx, build.ID)
			if err != nil {
				t.Fatal(err)
			}
			inits := []*m.Symbol{}
			for _, sym := range symbols {
				if sym.Name == "init" {
					inits = append(inits, sym)
				}
			}
			if len(inits) != 2 || inits[0].SymbolKeyHash == inits[1].SymbolKeyHash || inits[0].ID == inits[1].ID {
				t.Fatalf("init identity collision: %+v", inits)
			}
			for i, sym := range inits {
				got, err := store.GetSymbolByID(ctx, sym.ID)
				if err != nil || got.SymbolKeyHash != sym.SymbolKeyHash {
					t.Fatalf("GetSymbol: %+v %v", got, err)
				}
				rels, err := store.ListRelationsForSymbol(ctx, build.ID, sym.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(rels) != 1 {
					t.Fatalf("init graph: %+v", rels)
				}
				rel := rels[0]
				if rel.FromSymbolID == nil || *rel.FromSymbolID != sym.ID || rel.TargetName != []string{"A", "B"}[i] || rel.ToSymbolID == nil {
					t.Fatalf("wrong init caller graph: %+v", rel)
				}
			}
			// Traversal across independent snapshots must not affect the discriminator.
			again, _ := analyzeCorrectnessFixture(t, files)
			for i, sym := range result.Symbols {
				if sym.SymbolKeyHash != again.Symbols[i].SymbolKeyHash {
					t.Fatal("non-deterministic declaration identity")
				}
			}
		})
	}
}

func TestSymbolIdentityCollisionRollsBackAnalysis(t *testing.T) {
	result, _ := analyzeCorrectnessFixture(t, map[string]string{"main.go": "package correctness\nfunc A() {}\nfunc B() { A() }\n"})
	result.Symbols[1].SymbolKeyHash = result.Symbols[0].SymbolKeyHash
	db, _, store, _ := setupCodeIntelTestDB(t)
	ctx := context.Background()
	build, _, err := store.GetOrCreateBuild(ctx, "snap", result.ModulePath, codeintel.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if err = store.MarkBuildBuilding(ctx, build.ID); err != nil {
		t.Fatal(err)
	}
	err = store.SaveAnalysisResult(ctx, build.ID, result)
	if err == nil || !strings.Contains(err.Error(), "symbol identity collision") {
		t.Fatalf("collision not rejected: %v", err)
	}
	for _, table := range []string{"code_files", "symbols", "symbol_relations"} {
		var count int64
		if err := db.Table(table).Where("code_index_build_id = ?", build.ID).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("partial graph in %s: %d", table, count)
		}
	}
	got, err := store.GetByID(ctx, build.ID)
	if err != nil || got.Status != m.BuildStatusBuilding {
		t.Fatalf("build published on collision: %+v %v", got, err)
	}
}

func TestCurrentBuildDoesNotReuseHistoricalIdentity(t *testing.T) {
	db, _, store, _ := setupCodeIntelTestDB(t)
	ctx := context.Background()
	bc := codeintel.DefaultBuildContext()
	old := &m.CodeIndexBuild{SnapshotID: "snap", ModulePath: "example.com/correctness", ParserVersion: "v2.2.2", AnalyzerVersion: "v2.2.2", SymbolSchemaVersion: "v2.1.1", BuildContextHash: bc.BuildContextHash(), Status: m.BuildStatusReady}
	if err := db.Create(old).Error; err != nil {
		t.Fatal(err)
	}
	current, created, err := store.GetOrCreateBuild(ctx, "snap", old.ModulePath, bc)
	if err != nil || !created || current.ID == old.ID {
		t.Fatalf("stale build reused: %+v %v", current, err)
	}
	historical, err := store.GetByID(ctx, old.ID)
	if err != nil || historical.Status != m.BuildStatusReady {
		t.Fatalf("historical build unreadable: %+v %v", historical, err)
	}
	oldRetrieval, _, err := store.GetOrCreateRetrievalBuild(ctx, old.ID, m.ProductionRetrievalStrategy)
	if err != nil {
		t.Fatal(err)
	}
	newRetrieval, created, err := store.GetOrCreateRetrievalBuild(ctx, current.ID, m.ProductionRetrievalStrategy)
	if err != nil || !created || oldRetrieval.ID == newRetrieval.ID || newRetrieval.CodeIndexBuildID != current.ID {
		t.Fatalf("retrieval lineage reused: %+v %v", newRetrieval, err)
	}
}
