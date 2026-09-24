package repo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
)

type triggerIndexRepoStore struct {
	*fakeStore
	repository *Repository
}

func (s *triggerIndexRepoStore) GetByIDAndUser(_ context.Context, id, userID string) (*Repository, error) {
	if id != s.repository.ID || userID != s.repository.UserID {
		return nil, gorm.ErrRecordNotFound
	}
	copy := *s.repository
	return &copy, nil
}

type recordingSnapshotResolver struct {
	calls  int
	gitURL string
	ref    string
}

func (r *recordingSnapshotResolver) ResolveRef(_ context.Context, gitURL, ref string) (string, error) {
	r.calls++
	r.gitURL = gitURL
	r.ref = ref
	return "0123456789abcdef0123456789abcdef01234567", nil
}

func newTriggerIndexTestHandler(t *testing.T) (*gin.Engine, *gorm.DB, *recordingSnapshotResolver) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "trigger-index.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	if err := db.AutoMigrate(&Repository{}, &snapshot.RepositorySnapshot{}, &jobs.AnalysisJob{}); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get SQL database: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	store := &triggerIndexRepoStore{
		fakeStore: &fakeStore{},
		repository: &Repository{
			ID:         "repo-1",
			UserID:     "user-1",
			Name:       "repo",
			GitURL:     "https://example.com/repo.git",
			DefaultRef: "release/2.x",
			Status:     StatusActive,
		},
	}
	resolver := &recordingSnapshotResolver{}
	handler := NewHandler(NewService(store), snapshot.NewStore(db), nil, db).
		WithSnapshotResolver(resolver, jobs.NewStoreWithDriver(sqlDB, "sqlite"))
	router := gin.New()
	router.POST("/repositories/:id/index", func(c *gin.Context) {
		c.Set(string(logger.UserIDKey), "user-1")
		handler.TriggerIndex(c)
	})
	return router, db, resolver
}

func TestTriggerIndexRejectsInvalidJSONWithoutSideEffects(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "malformed JSON", body: `{"ref":`},
		{name: "wrong field type", body: `{"ref":123}`},
		{name: "null ref", body: `{"ref":null}`},
		{name: "null strategy", body: `{"strategy":null}`},
		{name: "trailing JSON document", body: `{} {}`},
		{name: "unknown field", body: `{"branch":"main"}`},
		{name: "case-mismatched field name", body: `{"REF":"feature/other"}`},
		{name: "duplicate field", body: `{"ref":"main","ref":"other"}`},
		{name: "invalid UTF-8", body: string([]byte{'{', '"', 'r', 'e', 'f', '"', ':', '"', 'x', 0xff, '"', '}'})},
		{name: "non-object JSON", body: `null`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, db, resolver := newTriggerIndexTestHandler(t)
			request := httptest.NewRequest(http.MethodPost, "/repositories/repo-1/index", strings.NewReader(tt.body))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusBadRequest, response.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if payload["code"] != "INPUT_INVALID" {
				t.Fatalf("error code = %v, want INPUT_INVALID", payload["code"])
			}
			if payload["error"] != "invalid index request" {
				t.Fatalf("error message = %v, want stable sanitized message", payload["error"])
			}
			if len(payload) != 2 {
				t.Fatalf("response leaked parser details or unexpected fields: %v", payload)
			}
			if resolver.calls != 0 {
				t.Fatalf("ResolveRef called %d times for invalid request", resolver.calls)
			}

			var snapshotCount, jobCount int64
			if err := db.Model(&snapshot.RepositorySnapshot{}).Count(&snapshotCount).Error; err != nil {
				t.Fatalf("count snapshots: %v", err)
			}
			if err := db.Model(&jobs.AnalysisJob{}).Count(&jobCount).Error; err != nil {
				t.Fatalf("count jobs: %v", err)
			}
			if snapshotCount != 0 || jobCount != 0 {
				t.Fatalf("invalid request side effects: snapshots=%d jobs=%d", snapshotCount, jobCount)
			}
		})
	}
}

func TestTriggerIndexDefaultsValidEmptyRequests(t *testing.T) {
	tests := []struct {
		name string
		body string
		ref  string
	}{
		{name: "empty object", body: `{}`, ref: "release/2.x"},
		{name: "empty body EOF compatibility", body: "", ref: "release/2.x"},
		{name: "explicit ref", body: `{"ref":"feature/fix","strategy":"BM25"}`, ref: "feature/fix"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			router, db, resolver := newTriggerIndexTestHandler(t)
			request := httptest.NewRequest(http.MethodPost, "/repositories/repo-1/index", strings.NewReader(tt.body))
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)

			if response.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, http.StatusAccepted, response.Body.String())
			}
			if resolver.calls != 1 || resolver.ref != tt.ref {
				t.Fatalf("ResolveRef calls/ref = %d/%q, want 1/%q", resolver.calls, resolver.ref, tt.ref)
			}
			var savedSnapshot snapshot.RepositorySnapshot
			if err := db.First(&savedSnapshot).Error; err != nil {
				t.Fatalf("load queued snapshot: %v", err)
			}
			if savedSnapshot.Ref != tt.ref {
				t.Fatalf("snapshot ref = %q, want %q", savedSnapshot.Ref, tt.ref)
			}
			var jobCount int64
			if err := db.Model(&jobs.AnalysisJob{}).Count(&jobCount).Error; err != nil {
				t.Fatalf("count jobs: %v", err)
			}
			if jobCount != 1 {
				t.Fatalf("job count = %d, want 1", jobCount)
			}
		})
	}
}

func TestTriggerIndexDefaultsUseRepositoryRefAndBM25(t *testing.T) {
	req := TriggerIndexRequest{}
	applyTriggerIndexDefaults(&req, "release/2.x")
	if req.Ref != "release/2.x" {
		t.Fatalf("ref = %q, want repository default ref", req.Ref)
	}
	if req.Strategy != repoindex.StrategyBM25 {
		t.Fatalf("strategy = %q, want BM25", req.Strategy)
	}
}
