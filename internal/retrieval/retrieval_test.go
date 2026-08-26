package retrieval_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/platform/mysql"
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
	_ = ciStore.CompleteRetrievalBuild(ctx, rb.ID, finalPath, hash, idx.TotalDocs)

	// Search using ProductionRetriever
	retriever := retrieval.NewProductionRetriever(ciStore, tempBase)
	results, err := retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: snapID,
		Query:      "ValidateToken",
		TopK:       5,
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
