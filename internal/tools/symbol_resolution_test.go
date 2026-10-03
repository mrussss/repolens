package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
)

func setupSymbolResolutionTest(t *testing.T) (context.Context, codeintelstore.Store, []*codeintelmodel.Symbol) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "symbols.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&codeintelmodel.Symbol{}, &codeintelmodel.SymbolRelation{}); err != nil {
		t.Fatal(err)
	}
	symbols := []*codeintelmodel.Symbol{
		{CodeIndexBuildID: 1, SymbolKeyHash: "hash-a", ModulePath: "example.com/app", PackagePath: "pkg/a", PackageName: "a", Kind: codeintelmodel.SymbolKindMethod, Name: "Run", QualifiedName: "Service.Run", FilePath: "pkg/a/service.go"},
		{CodeIndexBuildID: 1, SymbolKeyHash: "hash-b", ModulePath: "example.com/app", PackagePath: "pkg/b", PackageName: "b", Kind: codeintelmodel.SymbolKindMethod, Name: "Run", QualifiedName: "Worker.Run", FilePath: "pkg/b/worker.go"},
		{CodeIndexBuildID: 1, SymbolKeyHash: "hash-c", ModulePath: "example.com/app", PackagePath: "pkg/c", PackageName: "c", Kind: codeintelmodel.SymbolKindFunction, Name: "Run", QualifiedName: "Runner.Run", FilePath: "pkg/c/runner.go"},
		{CodeIndexBuildID: 1, SymbolKeyHash: "hash-only", ModulePath: "example.com/app", PackagePath: "pkg/only", PackageName: "only", Kind: codeintelmodel.SymbolKindFunction, Name: "Unique", QualifiedName: "Unique", FilePath: "pkg/only/unique.go"},
		{CodeIndexBuildID: 2, SymbolKeyHash: "hash-other-build", ModulePath: "example.com/other", PackagePath: "pkg/other", PackageName: "other", Kind: codeintelmodel.SymbolKindFunction, Name: "Run", QualifiedName: "Other.Run", FilePath: "pkg/other/run.go"},
	}
	for _, symbol := range symbols {
		if err := db.Create(symbol).Error; err != nil {
			t.Fatal(err)
		}
	}
	return context.Background(), codeintelstore.NewStore(db), symbols
}

func TestFindReferencesRejectsAmbiguousSymbolName(t *testing.T) {
	ctx, store, symbols := setupSymbolResolutionTest(t)
	result, err := NewFindReferencesTool(store, 1).Execute(ctx, `{"symbol_name":"Run"}`)
	if err != nil {
		t.Fatal(err)
	}
	assertSymbolFailure(t, result, "AMBIGUOUS_SYMBOL", 3)
	for _, symbol := range symbols[:3] {
		if !containsString(result, symbol.SymbolKeyHash) || !containsString(result, symbol.QualifiedName) {
			t.Fatalf("ambiguity response omitted candidate identity %q: %s", symbol.SymbolKeyHash, result)
		}
	}
}

func TestFindRelatedTestsRejectsAmbiguousSymbolName(t *testing.T) {
	ctx, store, _ := setupSymbolResolutionTest(t)
	result, err := NewFindRelatedTestsTool(store, 1).Execute(ctx, `{"symbol_name":"Run"}`)
	if err != nil {
		t.Fatal(err)
	}
	assertSymbolFailure(t, result, "AMBIGUOUS_SYMBOL", 3)
}

func TestGetSymbolDoesNotFallbackFromInvalidHash(t *testing.T) {
	ctx, store, _ := setupSymbolResolutionTest(t)
	result, err := NewGetSymbolTool(store, 1).Execute(ctx, `{"symbol_key_hash":"stale-hash","name":"Unique"}`)
	if err != nil {
		t.Fatal(err)
	}
	assertSymbolFailure(t, result, "SYMBOL_NOT_FOUND", 0)
}

func TestSymbolIdentityExactResolution(t *testing.T) {
	ctx, store, symbols := setupSymbolResolutionTest(t)
	tool := NewGetSymbolTool(store, 1)
	cases := []struct {
		name string
		args string
		want string
	}{
		{name: "hash exact lookup", args: `{"symbol_key_hash":"hash-b","name":"Unique"}`, want: "hash-b"},
		{name: "qualified name exact", args: `{"qualified_name":"Runner.Run"}`, want: "hash-c"},
		{name: "single plain name exact", args: `{"symbol_name":"Unique"}`, want: "hash-only"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.Execute(ctx, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			var symbol codeintelmodel.Symbol
			if err := json.Unmarshal([]byte(result), &symbol); err != nil {
				t.Fatalf("decode exact Symbol %q: %v", result, err)
			}
			if symbol.SymbolKeyHash != tc.want {
				t.Fatalf("resolved hash=%q, want %q: %s", symbol.SymbolKeyHash, tc.want, result)
			}
		})
	}

	idArgs, _ := json.Marshal(map[string]interface{}{"symbol_id": symbols[0].ID})
	idResult, err := tool.Execute(ctx, string(idArgs))
	if err != nil {
		t.Fatal(err)
	}
	var idSymbol codeintelmodel.Symbol
	if err := json.Unmarshal([]byte(idResult), &idSymbol); err != nil || idSymbol.ID != symbols[0].ID {
		t.Fatalf("exact symbol_id result=%s err=%v", idResult, err)
	}

	mismatchArgs, _ := json.Marshal(map[string]interface{}{"symbol_id": symbols[4].ID, "symbol_name": "Unique"})
	mismatch, err := tool.Execute(ctx, string(mismatchArgs))
	if err != nil {
		t.Fatal(err)
	}
	assertSymbolFailure(t, mismatch, "SYMBOL_BUILD_MISMATCH", 0)

	missing, err := tool.Execute(ctx, `{"symbol_name":"Missing"}`)
	if err != nil {
		t.Fatal(err)
	}
	assertSymbolFailure(t, missing, "SYMBOL_NOT_FOUND", 0)
}

func assertSymbolFailure(t *testing.T, result, wantCode string, wantCandidates int) {
	t.Helper()
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Candidates []symbolResolutionCandidate `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(result), &response); err != nil {
		t.Fatalf("decode symbol resolution failure %q: %v", result, err)
	}
	if response.Error.Code != wantCode || len(response.Candidates) != wantCandidates {
		t.Fatalf("symbol resolution response=%s; want code=%s candidates=%d", result, wantCode, wantCandidates)
	}
}

func containsString(text, value string) bool {
	return value != "" && strings.Contains(text, value)
}
