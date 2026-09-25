package retrieval_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
	"repolens/internal/snapshot"
)

func setupRetrievalDB(t *testing.T) (*gorm.DB, codeintelstore.Store, snapshot.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "retrieval_test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed migrating db: %v", err)
	}
	ciStore := codeintelstore.NewStore(db)
	snapStore := snapshot.NewStore(db)
	return db, ciStore, snapStore
}

func TestRetrievalJobHandlerIndexesSymbolSourceBody(t *testing.T) {
	_, ciStore, snapStore := setupRetrievalDB(t)
	ctx := context.Background()
	storageDir := t.TempDir()
	snapshotBase := t.TempDir()
	storeFS := snapshotstore.NewLocalSnapshotStore(snapshotBase)
	const repoID = "repo-source-body"
	const snapID = "snap-source-body"
	sourceDir, err := storeFS.EnsureDir(repoID, snapID)
	if err != nil {
		t.Fatalf("create snapshot source directory: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(sourceDir, "pkg"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "pkg", "recovery.go"), []byte("package recovery\n\nfunc RunRecovery() error {\n\treturn errors.New(\"revision leaseexpiredqz unexpectedly\")\n}\n\nfunc BodyOnlyHelper() string {\n\treturn \"bodyindexmezz\"\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "pkg", "recovery_test.go"), []byte("package recovery\n\nfunc TestRunRecovery() {\n\tRunRecovery()\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapID, RepositoryID: repoID, CommitSHA: "source-body-commit", Ref: "main",
		MaterializedPath: sourceDir, ContentHash: "source-body-snapshot", Status: snapshot.StatusReady, ReadyAt: &now,
	}); err != nil {
		t.Fatalf("create snapshot record: %v", err)
	}

	build, _, err := ciStore.GetOrCreateBuild(ctx, snapID, "example.com/recovery", codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("create code index build: %v", err)
	}
	if err := ciStore.MarkBuildBuilding(ctx, build.ID); err != nil {
		t.Fatalf("mark code index build building: %v", err)
	}
	prodRaw, prodHash := codeintelmodel.BuildSymbolKey("example.com/recovery", "example.com/recovery/pkg", "", codeintelmodel.SymbolKindFunction, "RunRecovery")
	bodyRaw, bodyHash := codeintelmodel.BuildSymbolKey("example.com/recovery", "example.com/recovery/pkg", "", codeintelmodel.SymbolKindFunction, "BodyOnlyHelper")
	testRaw, testHash := codeintelmodel.BuildSymbolKey("example.com/recovery", "example.com/recovery/pkg", "", codeintelmodel.SymbolKindFunction, "TestRunRecovery")
	analysis := &codeintelmodel.AnalysisResult{
		ModulePath: "example.com/recovery", BuildContext: codeintelmodel.DefaultBuildContext(),
		Files: []*codeintelmodel.CodeFile{
			{Path: "pkg/recovery.go", PackagePath: "example.com/recovery/pkg", PackageName: "recovery", ContentHash: "production-file", LineCount: 9, ParseStatus: "OK"},
			{Path: "pkg/recovery_test.go", PackagePath: "example.com/recovery/pkg", PackageName: "recovery", ContentHash: "test-file", LineCount: 5, IsTest: true, ParseStatus: "OK"},
		},
		Symbols: []*codeintelmodel.Symbol{
			{FilePath: "pkg/recovery.go", SymbolKeyRaw: prodRaw, SymbolKeyHash: prodHash, ModulePath: "example.com/recovery", PackagePath: "example.com/recovery/pkg", PackageName: "recovery", Kind: codeintelmodel.SymbolKindFunction, Name: "RunRecovery", QualifiedName: "RunRecovery", Signature: "func RunRecovery() error", Doc: "Restores a user session.", StartLine: 3, EndLine: 5, ContentHash: "production-symbol"},
			{FilePath: "pkg/recovery.go", SymbolKeyRaw: bodyRaw, SymbolKeyHash: bodyHash, ModulePath: "example.com/recovery", PackagePath: "example.com/recovery/pkg", PackageName: "recovery", Kind: codeintelmodel.SymbolKindFunction, Name: "BodyOnlyHelper", QualifiedName: "BodyOnlyHelper", Signature: "func BodyOnlyHelper() string", StartLine: 7, EndLine: 9, ContentHash: "body-only-symbol"},
			{FilePath: "pkg/recovery_test.go", SymbolKeyRaw: testRaw, SymbolKeyHash: testHash, ModulePath: "example.com/recovery", PackagePath: "example.com/recovery/pkg", PackageName: "recovery", Kind: codeintelmodel.SymbolKindFunction, Name: "TestRunRecovery", QualifiedName: "TestRunRecovery", Signature: "func TestRunRecovery()", StartLine: 3, EndLine: 5, ContentHash: "test-symbol"},
		},
		RelatedTests: []*codeintelmodel.RelatedTestDiscovery{{
			TargetSymbolKeyHash: prodHash, TargetSymbolName: "RunRecovery", TestSymbolKeyHash: testHash,
			TestSymbolName: "TestRunRecovery", TestFilePath: "pkg/recovery_test.go", ReasonCode: codeintelmodel.TestReasonNameMatch,
			ResolutionKind: codeintelmodel.ResolutionKindHeuristic, Confidence: 0.7, Explanation: "name match", TestLine: 3,
		}},
	}
	if err := ciStore.SaveAnalysisResult(ctx, build.ID, analysis); err != nil {
		t.Fatalf("save analysis result: %v", err)
	}
	rb, _, err := ciStore.GetOrCreateRetrievalBuild(ctx, build.ID, "BM25")
	if err != nil {
		t.Fatalf("create retrieval build: %v", err)
	}
	handler := retrieval.NewRetrievalJobHandler(ciStore, storageDir).WithSnapshotSource(snapStore, storeFS)
	if err := handler.Execute(ctx, &jobs.AnalysisJob{ResourceID: fmt.Sprintf("%d", rb.ID)}); err != nil {
		t.Fatalf("execute retrieval build: %v", err)
	}
	retriever := retrieval.NewProductionRetriever(ciStore, storageDir)
	search := func(query string) []retrieval.SearchResult {
		t.Helper()
		results, err := retriever.Search(ctx, retrieval.SearchRequest{SnapshotID: snapID, CodeIndexBuildID: build.ID, RetrievalBuildID: rb.ID, Query: query, TopK: 5})
		if err != nil {
			t.Fatalf("search %q: %v", query, err)
		}
		return results
	}
	bodyResults := search("revision leaseexpiredqz unexpectedly")
	if len(bodyResults) == 0 || bodyResults[0].Path != "pkg/recovery.go" || bodyResults[0].Symbol != "RunRecovery" ||
		!strings.Contains(bodyResults[0].Snippet, "leaseexpiredqz") || bodyResults[0].SymbolKeys[0] != prodHash ||
		!strings.Contains(bodyResults[0].RetrievalReason, "RELATED_TEST_DISCOVERY") {
		t.Fatalf("body search failed to return structurally boosted source symbol: %+v", bodyResults)
	}
	bodyOnlyResults := search("bodyindexmezz")
	if len(bodyOnlyResults) == 0 || bodyOnlyResults[0].Path != "pkg/recovery.go" || bodyOnlyResults[0].Symbol != "BodyOnlyHelper" {
		t.Fatalf("second same-file symbol body was not indexed: %+v", bodyOnlyResults)
	}
	if results := search("RunRecovery"); len(results) == 0 || results[0].Symbol != "RunRecovery" {
		t.Fatalf("symbol-name search regressed: %+v", results)
	}
	if results := search("restores session"); len(results) == 0 || results[0].Symbol != "RunRecovery" {
		t.Fatalf("documentation search regressed: %+v", results)
	}
}

func TestRetrievalJobHandlerIndexesAllSymbols(t *testing.T) {
	_, ciStore, _ := setupRetrievalDB(t)
	ctx := context.Background()
	storageDir := t.TempDir()

	cib, _, err := ciStore.GetOrCreateBuild(ctx, "snap-many-symbols", "example.com/many", codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("create code index build: %v", err)
	}
	if err := ciStore.MarkBuildBuilding(ctx, cib.ID); err != nil {
		t.Fatalf("mark code index build building: %v", err)
	}

	const symbolCount = 125
	analysis := &codeintelmodel.AnalysisResult{
		ModulePath:   "example.com/many",
		BuildContext: codeintelmodel.DefaultBuildContext(),
		Files: []*codeintelmodel.CodeFile{{
			Path: "pkg/many.go", PackagePath: "example.com/many/pkg", PackageName: "many",
			ContentHash: "file-hash", LineCount: symbolCount * 3, ParseStatus: "OK",
		}},
		Symbols: make([]*codeintelmodel.Symbol, 0, symbolCount),
	}
	for i := 0; i < symbolCount; i++ {
		name := fmt.Sprintf("Symbol%03d", i)
		raw, hash := codeintelmodel.BuildSymbolKey("example.com/many", "example.com/many/pkg", "", codeintelmodel.SymbolKindFunction, name)
		analysis.Symbols = append(analysis.Symbols, &codeintelmodel.Symbol{
			FilePath: "pkg/many.go", SymbolKeyRaw: raw, SymbolKeyHash: hash,
			ModulePath: "example.com/many", PackagePath: "example.com/many/pkg", PackageName: "many",
			Kind: codeintelmodel.SymbolKindFunction, Name: name, QualifiedName: name,
			Signature: "func()", StartLine: i*3 + 1, EndLine: i*3 + 2, ContentHash: fmt.Sprintf("symbol-hash-%d", i),
		})
	}
	if err := ciStore.SaveAnalysisResult(ctx, cib.ID, analysis); err != nil {
		t.Fatalf("save analysis result: %v", err)
	}

	rb, _, err := ciStore.GetOrCreateRetrievalBuild(ctx, cib.ID, "BM25")
	if err != nil {
		t.Fatalf("create retrieval build: %v", err)
	}
	handler := retrieval.NewRetrievalJobHandler(ciStore, storageDir)
	if err := handler.Execute(ctx, &jobs.AnalysisJob{ResourceID: fmt.Sprintf("%d", rb.ID)}); err != nil {
		t.Fatalf("execute retrieval build: %v", err)
	}

	readyBuild, err := ciStore.GetRetrievalBuildByID(ctx, rb.ID)
	if err != nil {
		t.Fatalf("read retrieval build: %v", err)
	}
	if readyBuild.DocumentCount != symbolCount {
		t.Fatalf("document_count = %d, want %d", readyBuild.DocumentCount, symbolCount)
	}

	retriever := retrieval.NewProductionRetriever(ciStore, storageDir)
	results, err := retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: "snap-many-symbols", CodeIndexBuildID: cib.ID, RetrievalBuildID: rb.ID,
		Query: "Symbol124", TopK: 5,
	})
	if err != nil {
		t.Fatalf("search late symbol: %v", err)
	}
	if len(results) == 0 || results[0].Symbol != "Symbol124" || results[0].Path != "pkg/many.go" {
		t.Fatalf("last symbol was not searchable: %+v", results)
	}
}

func TestProductionRetriever_SearchAndStructuralExpansion(t *testing.T) {
	_, ciStore, snapStore := setupRetrievalDB(t)
	ctx := context.Background()
	tempBase := t.TempDir()

	snapID := "snap-ret-001"
	repoID := "repo-ret-001"
	now := time.Now().UTC()

	_ = snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID:           snapID,
		RepositoryID: repoID,
		CommitSHA:    "sha-ret-001",
		Ref:          "main",
		Status:       snapshot.StatusReady,
		ReadyAt:      &now,
	})

	// Create CodeIndexBuild
	cib, _, err := ciStore.GetOrCreateBuild(ctx, snapID, "example.com/ret", codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("failed creating code index build: %v", err)
	}

	// Save AnalysisResult
	analysisRes := &codeintelmodel.AnalysisResult{
		ModulePath:   "example.com/ret",
		BuildContext: codeintelmodel.DefaultBuildContext(),
		Files: []*codeintelmodel.CodeFile{
			{Path: "pkg/auth/jwt.go", PackagePath: "example.com/ret/pkg/auth", PackageName: "auth", ContentHash: "h1", LineCount: 30, ParseStatus: "OK"},
			{Path: "pkg/auth/jwt_test.go", PackagePath: "example.com/ret/pkg/auth", PackageName: "auth", ContentHash: "h2", LineCount: 20, ParseStatus: "OK"},
		},
		Symbols: []*codeintelmodel.Symbol{
			{
				FilePath:          "pkg/auth/jwt.go",
				SymbolKeyRaw:      "example.com/ret|example.com/ret/pkg/auth|TokenValidator|METHOD|ValidateToken",
				SymbolKeyHash:     "key-val-token",
				ModulePath:        "example.com/ret",
				PackagePath:       "example.com/ret/pkg/auth",
				PackageName:       "auth",
				Kind:              codeintelmodel.SymbolKindMethod,
				Name:              "ValidateToken",
				QualifiedName:     "TokenValidator.ValidateToken",
				ReceiverCanonical: "TokenValidator",
				Signature:         "func (v *TokenValidator) ValidateToken(token string) (*Claims, error)",
				StartLine:         10,
				EndLine:           25,
				ContentHash:       "ch1",
			},
		},
		Relations: []*codeintelmodel.SymbolRelation{
			{
				FromSymbolKeyHash: "key-val-token",
				RelationType:      codeintelmodel.RelationTypeTestRelation,
				ResolutionKind:    codeintelmodel.ResolutionKindSemantic,
				Confidence:        1.0,
				ReasonCode:        string(codeintelmodel.TestReasonDirectSemantic),
				TargetName:        "TestValidateToken",
				FilePath:          "pkg/auth/jwt_test.go",
				Line:              5,
				Column:            1,
			},
		},
		Quality: codeintelmodel.AnalysisQuality{
			FilesTotal:             2,
			FilesParsed:            2,
			PackagesTotal:          1,
			PackagesTypechecked:    1,
			SymbolsTotal:           1,
			SemanticRelationsCount: 1,
		},
	}
	_ = ciStore.MarkBuildBuilding(ctx, cib.ID)
	_ = ciStore.SaveAnalysisResult(ctx, cib.ID, analysisRes)

	// Create & Build BM25 Index
	idx := bm25.NewIndex(1.2, 0.75)
	idx.AddDocument(bm25.Document{
		FilePath:      "pkg/auth/jwt.go",
		StartLine:     10,
		EndLine:       25,
		Content:       "func (v *TokenValidator) ValidateToken(token string) (*Claims, error)",
		SymbolName:    "ValidateToken",
		SymbolKeyHash: "key-val-token",
		Kind:          "METHOD",
	})
	idx.Build()

	// Publish atomic artifact
	pub := artifact.NewPublisher(tempBase)
	rb, _, _ := ciStore.GetOrCreateRetrievalBuild(ctx, cib.ID, "BM25")
	finalPath, hash, err := pub.Publish(rb.ID, "token-1", "BM25", idx)
	if err != nil {
		t.Fatalf("failed publishing artifact: %v", err)
	}
	_ = ciStore.MarkRetrievalBuilding(ctx, rb.ID)
	_ = ciStore.CompleteRetrievalBuild(ctx, rb.ID, finalPath, hash, idx.TotalDocs)

	// Search using ProductionRetriever
	retriever := retrieval.NewProductionRetriever(ciStore, tempBase)
	results, err := retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: snapID, CodeIndexBuildID: cib.ID, RetrievalBuildID: rb.ID,
		Query: "ValidateToken", TopK: 5,
	})
	if err != nil {
		t.Fatalf("production search failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("expected search results for ValidateToken")
	}

	if results[0].Symbol != "ValidateToken" {
		t.Errorf("expected symbol ValidateToken, got %s", results[0].Symbol)
	}
	if results[0].RetrievalSource != "symbol_bm25_structural" {
		t.Errorf("expected source symbol_bm25_structural, got %s", results[0].RetrievalSource)
	}
}
