package diagnosis_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/mysql"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

func TestDiagnosisRequestUsesSharedV22Fixture(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to locate test file")
	}
	fixturePath := filepath.Join(filepath.Dir(currentFile), "..", "..", "contracts", "v2.2", "diagnosis-create.request.json")
	body, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var request struct {
		AnalysisRevisionID string `json:"analysis_revision_id"`
		IssueTitle         string `json:"issue_title"`
		CodeIndexBuildID   int64  `json:"code_index_build_id"`
		RetrievalBuildID   int64  `json:"retrieval_build_id"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatal(err)
	}
	if request.AnalysisRevisionID == "" || request.IssueTitle == "" || request.CodeIndexBuildID != 0 || request.RetrievalBuildID != 0 {
		t.Fatalf("shared fixture does not describe the v2.2 revision request: %+v", request)
	}
}

func TestDiagnosisRevisionSubmissionRequiresRepositoryOwnership(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{ID: "private-repo", UserID: "owner", Name: "private", GitURL: "https://github.com/example/private"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&revision.AnalysisRevision{ID: "private-revision", RepositoryID: "private-repo", CommitSHA: "0123456789012345678901234567890123456789", PipelineVersion: "v2.2", PipelineFingerprint: "fingerprint", Status: revision.StatusReady, Stage: revision.StageReady}).Error; err != nil {
		t.Fatal(err)
	}
	svc := diagnosis.NewService(diagnosis.NewStore(db), repoStore, snapshot.NewStore(db)).WithRevisionStore(revision.NewStore(db))
	_, _, err := svc.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID: "attacker", AnalysisRevisionID: "private-revision", IssueTitle: "should not access", IdempotencyKey: "ownership-key",
	})
	if !errors.Is(err, revision.ErrNotFound) {
		t.Fatalf("cross-user revision submission error = %v, want revision not found", err)
	}
}

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

func TestDiagnosisAttemptEndpointsEnforceRunAndAttemptOwnership(t *testing.T) {
	db := newDiagnosisHandlerTestDB(t)
	if err := db.Create(&diagnosis.DiagnosisRun{
		ID: "run-owner", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "owner run", IdempotencyKey: "owner-key", IdempotencyRequestHash: "owner-hash",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&diagnosis.DiagnosisRun{
		ID: "run-other", UserID: "user", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "other run", IdempotencyKey: "other-key", IdempotencyRequestHash: "other-hash",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&diagnosis.DiagnosisRun{
		ID: "run-private", UserID: "another-user", RepositoryID: "repo", SnapshotID: "snapshot",
		IssueTitle: "private run", IdempotencyKey: "private-key", IdempotencyRequestHash: "private-hash",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&diagnosis.DiagnosisAttempt{
		ID: "attempt-other", DiagnosisRunID: "run-other", AttemptNo: 1,
		Status: diagnosis.AttemptStatusRunning, StartedAt: time.Now().UTC(),
		HeartbeatAt: time.Now().UTC(), DeadlineAt: time.Now().UTC().Add(time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}

	svc := diagnosis.NewService(diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))
	router := diagnosisHandlerRouter(svc)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/diagnoses/run-private/attempts", nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !bytes.Contains(response.Body.Bytes(), []byte(`"DIAGNOSIS_NOT_FOUND"`)) {
		t.Fatalf("cross-user/missing attempt list response = %d %s", response.Code, response.Body.String())
	}

	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/diagnoses/run-owner/steps?attempt_id=attempt-other", nil)
	router.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound || !bytes.Contains(response.Body.Bytes(), []byte(`"ATTEMPT_NOT_FOUND"`)) {
		t.Fatalf("cross-run trace response = %d %s", response.Code, response.Body.String())
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
	router.GET("/diagnoses/:id/attempts", handler.ListAttempts)
	router.GET("/diagnoses/:id/steps", handler.GetSteps)
	return router
}
