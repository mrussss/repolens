package codeintel_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/platform/mysql"
	"repolens/internal/snapshot"
)

func setupHandlerTestServer(t *testing.T) (*gin.Engine, codeintelstore.Store, snapshot.Store) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codeintel_handler_test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed opening test db: %v", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed migrating db: %v", err)
	}

	ciStore := codeintelstore.NewStore(db)
	snapStore := snapshot.NewStore(db)
	handler := codeintel.NewHandler(ciStore, snapStore)

	r := gin.New()
	v1 := r.Group("/api/v1")
	{
		v1.POST("/snapshots/:id/code-index-builds", handler.TriggerCodeIndexBuild)
		v1.GET("/code-index-builds/:id", handler.GetCodeIndexBuild)
		v1.GET("/code-index-builds/:id/quality", handler.GetQuality)
		v1.GET("/code-index-builds/:id/symbols", handler.ListSymbols)
		v1.GET("/symbols/:id", handler.GetSymbol)
		v1.GET("/symbols/:id/references", handler.GetSymbolReferences)
		v1.GET("/symbols/:id/tests", handler.GetSymbolTests)
		v1.POST("/code-index-builds/:id/retrieval-builds", handler.TriggerRetrievalBuild)
		v1.GET("/retrieval-builds/:id", handler.GetRetrievalBuild)
	}

	return r, ciStore, snapStore
}

func TestCodeIntelHTTP_EndToEndEndpoints(t *testing.T) {
	router, ciStore, snapStore := setupHandlerTestServer(t)
	ctx := context.Background()

	// 1. Seed Snapshot
	now := time.Now().UTC()
	_ = snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID:           "snap-h-1",
		RepositoryID: "repo-h-1",
		CommitSHA:    "sha-h-1",
		Ref:          "main",
		Status:       snapshot.StatusReady,
		ReadyAt:      &now,
	})

	// 2. POST /api/v1/snapshots/snap-h-1/code-index-builds -> 202 Accepted
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/snapshots/snap-h-1/code-index-builds", nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202 Accepted, got %d: %s", w.Code, w.Body.String())
	}

	var postRes map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &postRes)
	buildData := postRes["code_index_build"].(map[string]interface{})
	buildID := int64(buildData["id"].(float64))

	// 3. GET /api/v1/code-index-builds/:id -> 200 OK
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/code-index-builds/1", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200, got %d: %s", w.Code, w.Body.String())
	}

	// 4. Save analysis results directly to DB to simulate build completion
	analysisRes := &codeintelmodel.AnalysisResult{
		ModulePath:   "example.com/h",
		BuildContext: codeintelmodel.DefaultBuildContext(),
		Files: []*codeintelmodel.CodeFile{
			{Path: "main.go", PackagePath: "example.com/h", PackageName: "main", ContentHash: "h1", LineCount: 20, ParseStatus: "OK"},
		},
		Symbols: []*codeintelmodel.Symbol{
			{
				FilePath:          "main.go",
				SymbolKeyRaw:      "example.com/h|example.com/h|Service|METHOD|Execute",
				SymbolKeyHash:     "keyhash-exec",
				ModulePath:        "example.com/h",
				PackagePath:       "example.com/h",
				PackageName:       "main",
				Kind:              codeintelmodel.SymbolKindMethod,
				Name:              "Execute",
				QualifiedName:     "Service.Execute",
				ReceiverCanonical: "Service",
				Signature:         "func (s *Service) Execute() error",
				StartLine:         10,
				EndLine:           15,
				ContentHash:       "ch1",
			},
		},
		Relations: []*codeintelmodel.SymbolRelation{
			{
				RelationType:        codeintelmodel.RelationTypeCallCandidate,
				ResolutionKind:      codeintelmodel.ResolutionKindSemantic,
				Confidence:          1.0,
				ReasonCode:          "CALL_CANDIDATE",
				TargetName:          "Execute",
				TargetPackagePath:   "example.com/h",
				TargetQualifiedName: "Service.Execute",
				FilePath:            "main.go",
				Line:                12,
				Column:              4,
			},
			{
				FromSymbolKeyHash: "keyhash-exec",
				RelationType:      codeintelmodel.RelationTypeTestRelation,
				ResolutionKind:    codeintelmodel.ResolutionKindSemantic,
				Confidence:        1.0,
				ReasonCode:        string(codeintelmodel.TestReasonDirectSemantic),
				TargetName:        "TestExecute",
				FilePath:          "main_test.go",
				Line:              5,
				Column:            1,
			},
		},
		Quality: codeintelmodel.AnalysisQuality{
			FilesTotal:             1,
			FilesParsed:            1,
			PackagesTotal:          1,
			PackagesTypechecked:    1,
			SymbolsTotal:           1,
			SemanticRelationsCount: 2,
		},
	}
	_ = ciStore.MarkBuildBuilding(ctx, buildID)
	_ = ciStore.SaveAnalysisResult(ctx, buildID, analysisRes)

	// 5. GET /api/v1/code-index-builds/:id/quality -> 200 OK
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/code-index-builds/1/quality", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for quality, got %d: %s", w.Code, w.Body.String())
	}
	var qRes map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &qRes)
	if qRes["parsed_pct"] != "100.0%" {
		t.Errorf("expected parsed_pct 100.0%%, got %v", qRes["parsed_pct"])
	}

	// 6. GET /api/v1/code-index-builds/:id/symbols -> 200 OK
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/code-index-builds/1/symbols?q=Execute", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for symbols, got %d: %s", w.Code, w.Body.String())
	}
	var symRes map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &symRes)
	symbolsList := symRes["symbols"].([]interface{})
	if len(symbolsList) != 1 {
		t.Fatalf("expected 1 symbol, got %d", len(symbolsList))
	}

	// 7. GET /api/v1/symbols/:id/tests -> 200 OK
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodGet, "/api/v1/symbols/1/tests?code_index_build_id=1&symbol_key_hash=keyhash-exec", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected HTTP 200 for symbol tests, got %d: %s", w.Code, w.Body.String())
	}
	var testRes map[string]interface{}
	_ = json.Unmarshal(w.Body.Bytes(), &testRes)
	testsList := testRes["related_tests"].([]interface{})
	if len(testsList) != 1 {
		t.Fatalf("expected 1 related test, got %d", len(testsList))
	}

	// 8. POST /api/v1/code-index-builds/:id/retrieval-builds -> 202 Accepted
	w = httptest.NewRecorder()
	req, _ = http.NewRequest(http.MethodPost, "/api/v1/code-index-builds/1/retrieval-builds", nil)
	router.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected HTTP 202 for retrieval build creation, got %d: %s", w.Code, w.Body.String())
	}
}
