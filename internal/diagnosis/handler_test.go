package diagnosis_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/mysql"
	"repolens/internal/repo"
	"repolens/internal/snapshot"
)

func TestDiagnosisCreateRejectsUnconfiguredProviderWithoutCreatingJob(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	diagStore := diagnosis.NewStore(db)
	svc := diagnosis.NewService(diagStore, repo.NewStore(db), snapshot.NewStore(db))
	svc.WithProviderMetadataSource(func() diagnosis.ProviderMetadata {
		return diagnosis.ProviderMetadata{IsConfigured: false}
	})

	router := diagnosisHandlerRouter(svc)
	body := []byte(`{"repository_id":"repo","snapshot_id":"snapshot","issue_title":"issue","code_index_build_id":1,"retrieval_build_id":2}`)
	req := httptest.NewRequest(http.MethodPost, "/diagnoses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "new-key")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)

	if response.Code != http.StatusFailedDependency {
		t.Fatalf("status = %d, want 424: %s", response.Code, response.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["code"] != "PROVIDER_NOT_CONFIGURED" {
		t.Fatalf("error code = %v", payload["code"])
	}
	var runCount, jobCount int64
	if err := db.Model(&diagnosis.DiagnosisRun{}).Count(&runCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Count(&jobCount).Error; err != nil {
		t.Fatal(err)
	}
	if runCount != 0 || jobCount != 0 {
		t.Fatalf("unconfigured request created run/job: %d/%d", runCount, jobCount)
	}
}

func TestDiagnosisHandlerGetDistinguishesNotFoundFromStoreFailure(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	svc := diagnosis.NewService(diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)

	response := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/diagnoses/missing", nil)
	router.ServeHTTP(response, req)
	if response.Code != http.StatusNotFound || !bytes.Contains(response.Body.Bytes(), []byte(`"DIAGNOSIS_NOT_FOUND"`)) {
		t.Fatalf("not found response = %d %s", response.Code, response.Body.String())
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/diagnoses/any", nil)
	router.ServeHTTP(response, req)
	if response.Code != http.StatusInternalServerError || !bytes.Contains(response.Body.Bytes(), []byte(`"INTERNAL_ERROR"`)) || bytes.Contains(response.Body.Bytes(), []byte("database")) {
		t.Fatalf("store failure response = %d %s", response.Code, response.Body.String())
	}
}

func TestDiagnosisHandlerPaginationValidation(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	svc := diagnosis.NewService(diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)

	tests := []struct {
		query string
		code  int
	}{
		{query: "", code: http.StatusOK},
		{query: "?page=2&page_size=50", code: http.StatusOK},
		{query: "?page=abc", code: http.StatusBadRequest},
		{query: "?page=0", code: http.StatusBadRequest},
		{query: "?page=-1", code: http.StatusBadRequest},
		{query: "?page_size=abc", code: http.StatusBadRequest},
		{query: "?page_size=0", code: http.StatusBadRequest},
		{query: "?page_size=101", code: http.StatusBadRequest},
	}
	for _, tt := range tests {
		response := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/diagnoses"+tt.query, nil)
		router.ServeHTTP(response, req)
		if response.Code != tt.code {
			t.Errorf("query %q status = %d, want %d: %s", tt.query, response.Code, tt.code, response.Body.String())
		}
		if tt.code == http.StatusBadRequest && !bytes.Contains(response.Body.Bytes(), []byte(`"INVALID_PAGINATION"`)) {
			t.Errorf("query %q missing INVALID_PAGINATION: %s", tt.query, response.Body.String())
		}
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/diagnoses?page=1", nil)
	router.ServeHTTP(response, req)
	if response.Code != http.StatusInternalServerError || !bytes.Contains(response.Body.Bytes(), []byte(`"INTERNAL_ERROR"`)) {
		t.Fatalf("list store failure response = %d %s", response.Code, response.Body.String())
	}
}

func TestDiagnosisCreateIdempotencyReplayPrecedesProviderCheck(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	snapshotStore := snapshot.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{ID: "repo-replay", UserID: "user", Name: "replay", GitURL: "https://github.com/example/replay"}); err != nil {
		t.Fatal(err)
	}
	if err := snapshotStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: "snapshot-replay", RepositoryID: "repo-replay", CommitSHA: "commit-replay",
		MaterializedPath: t.TempDir(), Status: snapshot.StatusReady,
	}); err != nil {
		t.Fatal(err)
	}

	configured := true
	svc := diagnosis.NewService(diagnosis.NewStore(db), repoStore, snapshotStore)
	svc.WithProviderMetadataSource(func() diagnosis.ProviderMetadata {
		return diagnosis.ProviderMetadata{IsConfigured: configured}
	})
	input := diagnosis.CreateDiagnosisInput{
		UserID: "user", RepositoryID: "repo-replay", SnapshotID: "snapshot-replay",
		IssueTitle: "issue", IdempotencyKey: "replay-key", CodeIndexBuildID: 11, RetrievalBuildID: 12,
	}
	run, created, err := svc.Create(ctx, input)
	if err != nil || !created {
		t.Fatalf("initial create failed: created=%v err=%v", created, err)
	}
	configured = false
	replayed, created, err := svc.Create(ctx, input)
	if err != nil || created || replayed.ID != run.ID {
		t.Fatalf("replay was not returned before provider check: created=%v run=%v err=%v", created, replayed, err)
	}
	input.IssueTitle = "different issue"
	if _, _, err := svc.Create(ctx, input); err != diagnosis.ErrIdempotencyConflict {
		t.Fatalf("conflict was not returned before provider check: %v", err)
	}
}

func newDiagnosisHandlerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "handler.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func diagnosisHandlerRouter(svc *diagnosis.Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(logger.UserIDKey), "user")
		c.Next()
	})
	handler := diagnosis.NewHandler(svc, nil, nil, nil)
	router.POST("/diagnoses", handler.Create)
	router.GET("/diagnoses", handler.List)
	router.GET("/diagnoses/:id", handler.Get)
	return router
}
