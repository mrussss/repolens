package codeintel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/snapshot"
)

func setupHandlerTestServer(t *testing.T) (*gin.Engine, codeintelstore.Store, snapshot.Store, *gorm.DB, *jobs.Store) {
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
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed getting sql.DB: %v", err)
	}
	jobStore := jobs.NewStoreWithDriver(sqlDB, "sqlite3")
	handler := codeintel.NewHandler(ciStore, snapStore).WithJobStore(jobStore)

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

	return r, ciStore, snapStore, db, jobStore
}

func TestCodeIntelHTTP_EndToEndEndpoints(t *testing.T) {
	router, ciStore, snapStore, _, _ := setupHandlerTestServer(t)
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

func TestSymbolHTTPIdentityUsesURLIDAndValidatesBuild(t *testing.T) {
	router, ciStore, _, db, _ := setupHandlerTestServer(t)
	ctx := context.Background()
	buildA, _, err := ciStore.GetOrCreateBuild(ctx, "snap-symbol-api-a", "example.com/api", codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	buildB, _, err := ciStore.GetOrCreateBuild(ctx, "snap-symbol-api-b", "example.com/api", codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	symbolA := &codeintelmodel.Symbol{CodeIndexBuildID: buildA.ID, SymbolKeyHash: "api-hash-a", ModulePath: "example.com/api", PackagePath: "pkg/a", PackageName: "a", Kind: codeintelmodel.SymbolKindFunction, Name: "A", QualifiedName: "A", FilePath: "pkg/a/a.go"}
	symbolB := &codeintelmodel.Symbol{CodeIndexBuildID: buildA.ID, SymbolKeyHash: "api-hash-b", ModulePath: "example.com/api", PackagePath: "pkg/b", PackageName: "b", Kind: codeintelmodel.SymbolKindFunction, Name: "B", QualifiedName: "B", FilePath: "pkg/b/b.go"}
	symbolOtherBuild := &codeintelmodel.Symbol{CodeIndexBuildID: buildB.ID, SymbolKeyHash: "api-hash-other", ModulePath: "example.com/api", PackagePath: "pkg/other", PackageName: "other", Kind: codeintelmodel.SymbolKindFunction, Name: "Other", QualifiedName: "Other", FilePath: "pkg/other/other.go"}
	for _, symbol := range []*codeintelmodel.Symbol{symbolA, symbolB, symbolOtherBuild} {
		if err := db.Create(symbol).Error; err != nil {
			t.Fatal(err)
		}
	}
	fromA, toB := symbolA.ID, symbolB.ID
	referenceA := &codeintelmodel.SymbolRelation{
		CodeIndexBuildID: buildA.ID, FromSymbolID: &fromA, ToSymbolID: &toB,
		FromSymbolKeyHash: symbolA.SymbolKeyHash, ToSymbolKeyHash: symbolB.SymbolKeyHash,
		RelationType: codeintelmodel.RelationTypeReference, ResolutionKind: codeintelmodel.ResolutionKindSemantic,
		ReasonCode: "API_REFERENCE", FilePath: "pkg/a/a.go", TargetName: "B",
	}
	testA := &codeintelmodel.SymbolRelation{
		CodeIndexBuildID: buildA.ID, FromSymbolID: &fromA,
		FromSymbolKeyHash: symbolA.SymbolKeyHash, ToSymbolKeyHash: "test-hash-a",
		RelationType: codeintelmodel.RelationTypeTestRelation, ResolutionKind: codeintelmodel.ResolutionKindSemantic,
		ReasonCode: "API_TEST", FilePath: "pkg/a/a_test.go", TargetName: "TestA",
	}
	testB := &codeintelmodel.SymbolRelation{
		CodeIndexBuildID: buildA.ID, FromSymbolID: &toB,
		FromSymbolKeyHash: symbolB.SymbolKeyHash, ToSymbolKeyHash: "test-hash-b",
		RelationType: codeintelmodel.RelationTypeTestRelation, ResolutionKind: codeintelmodel.ResolutionKindSemantic,
		ReasonCode: "API_TEST", FilePath: "pkg/b/b_test.go", TargetName: "TestB",
	}
	for _, relation := range []*codeintelmodel.SymbolRelation{referenceA, testA, testB} {
		if err := db.Create(relation).Error; err != nil {
			t.Fatal(err)
		}
	}
	request := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		req, err := http.NewRequest(http.MethodGet, path, nil)
		if err != nil {
			t.Fatal(err)
		}
		router.ServeHTTP(response, req)
		return response
	}

	got := request("/api/v1/symbols/" + strconv.FormatInt(symbolA.ID, 10))
	if got.Code != http.StatusOK {
		t.Fatalf("GetSymbol status=%d body=%s", got.Code, got.Body.String())
	}
	var fetched codeintelmodel.Symbol
	if err := json.Unmarshal(got.Body.Bytes(), &fetched); err != nil || fetched.ID != symbolA.ID || fetched.SymbolKeyHash != symbolA.SymbolKeyHash || fetched.QualifiedName != symbolA.QualifiedName {
		t.Fatalf("GetSymbol payload=%s err=%v", got.Body.String(), err)
	}
	if got := request("/api/v1/symbols/not-a-number"); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid symbol id status=%d body=%s; want 400", got.Code, got.Body.String())
	}
	if got := request("/api/v1/symbols/0"); got.Code != http.StatusBadRequest {
		t.Fatalf("zero symbol id status=%d body=%s; want 400", got.Code, got.Body.String())
	}
	if got := request("/api/v1/symbols/999999"); got.Code != http.StatusNotFound {
		t.Fatalf("missing symbol status=%d body=%s; want 404", got.Code, got.Body.String())
	}

	base := "/api/v1/symbols/" + strconv.FormatInt(symbolA.ID, 10)
	if got := request(base + "/references?code_index_build_id=" + strconv.FormatInt(buildA.ID, 10) + "&symbol_key_hash=" + symbolB.SymbolKeyHash); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "SYMBOL_IDENTITY_MISMATCH") {
		t.Fatalf("URL/query identity mismatch status=%d body=%s; want 400 SYMBOL_IDENTITY_MISMATCH", got.Code, got.Body.String())
	}
	if got := request(base + "/references?code_index_build_id=not-a-number"); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid references build id status=%d body=%s; want 400", got.Code, got.Body.String())
	}
	if got := request(base + "/references?code_index_build_id=" + strconv.FormatInt(buildB.ID, 10)); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "SYMBOL_BUILD_MISMATCH") {
		t.Fatalf("references build mismatch status=%d body=%s; want 400 SYMBOL_BUILD_MISMATCH", got.Code, got.Body.String())
	}
	if got := request(base + "/tests?code_index_build_id=not-a-number"); got.Code != http.StatusBadRequest {
		t.Fatalf("invalid tests build id status=%d body=%s; want 400", got.Code, got.Body.String())
	}
	if got := request(base + "/tests?code_index_build_id=" + strconv.FormatInt(buildB.ID, 10)); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "SYMBOL_BUILD_MISMATCH") {
		t.Fatalf("tests build mismatch status=%d body=%s; want 400 SYMBOL_BUILD_MISMATCH", got.Code, got.Body.String())
	}
	if got := request(base + "/tests?symbol_key_hash=" + symbolB.SymbolKeyHash); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "SYMBOL_IDENTITY_MISMATCH") {
		t.Fatalf("tests URL/query identity mismatch status=%d body=%s; want 400 SYMBOL_IDENTITY_MISMATCH", got.Code, got.Body.String())
	}
	got = request(base + "/references")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "API_REFERENCE") {
		t.Fatalf("references did not use URL Symbol identity: status=%d body=%s", got.Code, got.Body.String())
	}
	got = request(base + "/tests")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "test-hash-a") || strings.Contains(got.Body.String(), "test-hash-b") {
		t.Fatalf("tests did not use URL Symbol identity: status=%d body=%s", got.Code, got.Body.String())
	}
	got = request("/api/v1/symbols/" + strconv.FormatInt(symbolB.ID, 10) + "/tests")
	if got.Code != http.StatusOK || !strings.Contains(got.Body.String(), "test-hash-b") || strings.Contains(got.Body.String(), "test-hash-a") {
		t.Fatalf("tests for Symbol B did not use URL identity: status=%d body=%s", got.Code, got.Body.String())
	}
}

func TestTriggerCodeIndexBuildRequeuesOnlyRetryableLegacyFailures(t *testing.T) {
	router, ciStore, snapStore, db, jobStore := setupHandlerTestServer(t)
	ctx := context.Background()
	retryable := jobs.TerminalReasonRetryableExhausted
	permanent := jobs.TerminalReasonPermanent
	cases := []struct {
		name            string
		buildStatus     codeintelmodel.BuildStatus
		jobStatus       jobs.JobStatus
		terminalReason  *jobs.TerminalReason
		revisionID      string
		wantHTTP        int
		wantBuildStatus codeintelmodel.BuildStatus
		wantJobStatus   jobs.JobStatus
	}{
		{name: "ready reuse", buildStatus: codeintelmodel.BuildStatusReady, wantHTTP: http.StatusOK, wantBuildStatus: codeintelmodel.BuildStatusReady},
		{name: "building reuse", buildStatus: codeintelmodel.BuildStatusBuilding, wantHTTP: http.StatusOK, wantBuildStatus: codeintelmodel.BuildStatusBuilding},
		{name: "created reuse", buildStatus: codeintelmodel.BuildStatusCreated, wantHTTP: http.StatusOK, wantBuildStatus: codeintelmodel.BuildStatusCreated},
		{name: "retryable exhausted", buildStatus: codeintelmodel.BuildStatusFailed, jobStatus: jobs.StatusFailed, terminalReason: &retryable, wantHTTP: http.StatusAccepted, wantBuildStatus: codeintelmodel.BuildStatusCreated, wantJobStatus: jobs.StatusPending},
		{name: "permanent failure", buildStatus: codeintelmodel.BuildStatusFailed, jobStatus: jobs.StatusFailed, terminalReason: &permanent, wantHTTP: http.StatusConflict, wantBuildStatus: codeintelmodel.BuildStatusFailed, wantJobStatus: jobs.StatusFailed},
		{name: "revision retry required", buildStatus: codeintelmodel.BuildStatusFailed, jobStatus: jobs.StatusFailed, terminalReason: &retryable, revisionID: "revision-retry-through-revision-api", wantHTTP: http.StatusConflict, wantBuildStatus: codeintelmodel.BuildStatusFailed, wantJobStatus: jobs.StatusFailed},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			snapshotID := fmt.Sprintf("snap-trigger-code-%d", index)
			if err := snapStore.Create(ctx, &snapshot.RepositorySnapshot{
				ID: snapshotID, RepositoryID: "repo-trigger-code", CommitSHA: fmt.Sprintf("commit-%d", index),
				Ref: "main", Status: snapshot.StatusReady,
			}); err != nil {
				t.Fatal(err)
			}
			build, _, err := ciStore.GetOrCreateBuild(ctx, snapshotID, "example.com/trigger", codeintelmodel.DefaultBuildContext())
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", build.ID).Updates(map[string]interface{}{
				"status": tc.buildStatus, "analysis_revision_id": tc.revisionID,
			}).Error; err != nil {
				t.Fatal(err)
			}
			job, err := jobStore.GetJobByResource(ctx, jobs.JobTypeBuildCodeIndex, strconv.FormatInt(build.ID, 10))
			if err != nil {
				t.Fatal(err)
			}
			if tc.jobStatus != "" {
				if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
					"status": tc.jobStatus, "terminal_reason": tc.terminalReason,
				}).Error; err != nil {
					t.Fatal(err)
				}
			}

			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/snapshots/"+snapshotID+"/code-index-builds", nil)
			router.ServeHTTP(response, request)
			if response.Code != tc.wantHTTP {
				t.Fatalf("HTTP status = %d, want %d: %s", response.Code, tc.wantHTTP, response.Body.String())
			}
			currentBuild, err := ciStore.GetByID(ctx, build.ID)
			if err != nil {
				t.Fatal(err)
			}
			currentJob, err := jobStore.GetJobByResource(ctx, jobs.JobTypeBuildCodeIndex, strconv.FormatInt(build.ID, 10))
			if err != nil {
				t.Fatal(err)
			}
			if currentBuild.Status != tc.wantBuildStatus || (tc.wantJobStatus != "" && currentJob.Status != tc.wantJobStatus) {
				t.Fatalf("after trigger build/job = %s/%s, want %s/%s", currentBuild.Status, currentJob.Status, tc.wantBuildStatus, tc.wantJobStatus)
			}
		})
	}
}

func TestTriggerRetrievalBuildRequeuesOnlyRetryableLegacyFailures(t *testing.T) {
	router, ciStore, _, db, jobStore := setupHandlerTestServer(t)
	ctx := context.Background()
	retryable := jobs.TerminalReasonRetryableExhausted
	permanent := jobs.TerminalReasonPermanent
	cases := []struct {
		name            string
		buildStatus     codeintelmodel.BuildStatus
		jobStatus       jobs.JobStatus
		terminalReason  *jobs.TerminalReason
		revisionID      string
		wantHTTP        int
		wantBuildStatus codeintelmodel.BuildStatus
		wantJobStatus   jobs.JobStatus
	}{
		{name: "ready reuse", buildStatus: codeintelmodel.BuildStatusReady, wantHTTP: http.StatusOK, wantBuildStatus: codeintelmodel.BuildStatusReady},
		{name: "building reuse", buildStatus: codeintelmodel.BuildStatusBuilding, wantHTTP: http.StatusOK, wantBuildStatus: codeintelmodel.BuildStatusBuilding},
		{name: "created reuse", buildStatus: codeintelmodel.BuildStatusCreated, wantHTTP: http.StatusOK, wantBuildStatus: codeintelmodel.BuildStatusCreated},
		{name: "retryable exhausted", buildStatus: codeintelmodel.BuildStatusFailed, jobStatus: jobs.StatusFailed, terminalReason: &retryable, wantHTTP: http.StatusAccepted, wantBuildStatus: codeintelmodel.BuildStatusCreated, wantJobStatus: jobs.StatusPending},
		{name: "permanent failure", buildStatus: codeintelmodel.BuildStatusFailed, jobStatus: jobs.StatusFailed, terminalReason: &permanent, wantHTTP: http.StatusConflict, wantBuildStatus: codeintelmodel.BuildStatusFailed, wantJobStatus: jobs.StatusFailed},
		{name: "revision retry required", buildStatus: codeintelmodel.BuildStatusFailed, jobStatus: jobs.StatusFailed, terminalReason: &retryable, revisionID: "revision-retry-through-revision-api", wantHTTP: http.StatusConflict, wantBuildStatus: codeintelmodel.BuildStatusFailed, wantJobStatus: jobs.StatusFailed},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			codeBuild, _, err := ciStore.GetOrCreateBuild(ctx, fmt.Sprintf("snap-trigger-retrieval-%d", index), "example.com/trigger", codeintelmodel.DefaultBuildContext())
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", codeBuild.ID).Update("status", codeintelmodel.BuildStatusReady).Error; err != nil {
				t.Fatal(err)
			}
			retrievalBuild, _, err := ciStore.GetOrCreateRetrievalBuild(ctx, codeBuild.ID, "BM25")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", retrievalBuild.ID).Updates(map[string]interface{}{
				"status": tc.buildStatus, "analysis_revision_id": tc.revisionID,
			}).Error; err != nil {
				t.Fatal(err)
			}
			job, err := jobStore.GetJobByResource(ctx, jobs.JobTypeBuildRetrieval, strconv.FormatInt(retrievalBuild.ID, 10))
			if err != nil {
				t.Fatal(err)
			}
			if tc.jobStatus != "" {
				if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
					"status": tc.jobStatus, "terminal_reason": tc.terminalReason,
				}).Error; err != nil {
					t.Fatal(err)
				}
			}

			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/code-index-builds/%d/retrieval-builds", codeBuild.ID), nil)
			router.ServeHTTP(response, request)
			if response.Code != tc.wantHTTP {
				t.Fatalf("HTTP status = %d, want %d: %s", response.Code, tc.wantHTTP, response.Body.String())
			}
			currentBuild, err := ciStore.GetRetrievalBuildByID(ctx, retrievalBuild.ID)
			if err != nil {
				t.Fatal(err)
			}
			currentJob, err := jobStore.GetJobByResource(ctx, jobs.JobTypeBuildRetrieval, strconv.FormatInt(retrievalBuild.ID, 10))
			if err != nil {
				t.Fatal(err)
			}
			if currentBuild.Status != tc.wantBuildStatus || (tc.wantJobStatus != "" && currentJob.Status != tc.wantJobStatus) {
				t.Fatalf("after trigger build/job = %s/%s, want %s/%s", currentBuild.Status, currentJob.Status, tc.wantBuildStatus, tc.wantJobStatus)
			}
		})
	}
}
