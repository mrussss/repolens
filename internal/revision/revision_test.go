package revision_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/mysql"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

type fixedResolver struct{ sha string }

func (r fixedResolver) ResolveRef(context.Context, string, string) (string, error) { return r.sha, nil }

type failingResolver struct{}

func (failingResolver) ResolveRef(context.Context, string, string) (string, error) {
	return "", errors.New("remote ref does not exist")
}

func newRevisionDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "revision.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func TestPrepareCreatesOneProductRevisionAndLineage(t *testing.T) {
	db := newRevisionDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	repository := &repo.Repository{ID: "repo-revision", UserID: "local-user", Name: "fixture", GitURL: "https://github.com/example/fixture", DefaultRef: "main"}
	if err := repoStore.Create(ctx, repository); err != nil {
		t.Fatal(err)
	}

	store := revision.NewStore(db)
	service := revision.NewService(store, repoStore, fixedResolver{sha: "0123456789012345678901234567890123456789"}, t.TempDir())
	first, created, err := service.Prepare(ctx, "local-user", repository.ID, "main")
	if err != nil || !created {
		t.Fatalf("first prepare = created=%v err=%v", created, err)
	}
	if first.Status != revision.StatusPreparing || first.Stage != revision.StageMaterializing || first.SnapshotID == "" || first.CodeIndexBuildID == 0 || first.RetrievalBuildID == 0 {
		t.Fatalf("incomplete revision lineage: %+v", first)
	}

	second, created, err := service.Prepare(ctx, "local-user", repository.ID, "feature")
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("same commit was not reused: created=%v first=%s second=%+v err=%v", created, first.ID, second, err)
	}
	var jobCount int64
	if err := db.Model(&jobs.AnalysisJob{}).Count(&jobCount).Error; err != nil {
		t.Fatal(err)
	}
	if jobCount != 3 {
		t.Fatalf("job count = %d, want one job per preparation stage", jobCount)
	}

	snap, err := snapshot.NewStore(db).GetByID(ctx, first.SnapshotID)
	if err != nil || snap.AnalysisRevisionID != first.ID {
		t.Fatalf("snapshot lineage = %+v err=%v", snap, err)
	}
}

func TestRevisionTransitionsToReadyAndRetryIsExplicit(t *testing.T) {
	db := newRevisionDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{ID: "repo-state", UserID: "user", Name: "state", GitURL: "https://github.com/example/state"}); err != nil {
		t.Fatal(err)
	}
	store := revision.NewStore(db)
	service := revision.NewService(store, repoStore, fixedResolver{sha: "abcdefabcdefabcdefabcdefabcdefabcdefabcd"}, t.TempDir())
	value, _, err := service.Prepare(ctx, "user", "repo-state", "main")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&snapshot.RepositorySnapshot{}).Where("id = ?", value.SnapshotID).Update("status", snapshot.StatusReady).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSnapshotReady(ctx, value.ID, value.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", value.CodeIndexBuildID).Update("status", codeintelmodel.BuildStatusReady).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCodeIndexReady(ctx, value.ID, value.CodeIndexBuildID); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", value.RetrievalBuildID).Update("status", codeintelmodel.BuildStatusReady).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRetrievalReady(ctx, value.ID, value.RetrievalBuildID); err != nil {
		t.Fatal(err)
	}
	ready, err := store.GetByID(ctx, value.ID)
	if err != nil || ready.Status != revision.StatusReady || ready.Stage != revision.StageReady {
		t.Fatalf("ready revision = %+v err=%v", ready, err)
	}
	if err := store.MarkFailed(ctx, value.ID, revision.StageFailed, "TEST", "failure"); err == nil {
		t.Fatal("READY revision was allowed to fail")
	}
}

func TestRevisionHandlerSeparatesRefFailureFromStoreFailure(t *testing.T) {
	db := newRevisionDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{ID: "repo-handler", UserID: "user-handler", Name: "handler", GitURL: "https://github.com/example/handler", DefaultRef: "main"}); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(string(logger.UserIDKey), "user-handler")
		c.Next()
	})
	handler := revision.NewHandler(revision.NewService(revision.NewStore(db), repoStore, failingResolver{}, t.TempDir()))
	router.POST("/repositories/:id/revisions", handler.Create)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/repositories/repo-handler/revisions", bytes.NewBufferString(`{"ref":"missing"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !bytes.Contains(response.Body.Bytes(), []byte(`"REF_NOT_FOUND"`)) {
		t.Fatalf("ref failure response = %d %s", response.Code, response.Body.String())
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/repositories/repo-handler/revisions", bytes.NewBufferString(`{"ref":"main"}`))
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || !bytes.Contains(response.Body.Bytes(), []byte(`"INTERNAL_ERROR"`)) {
		t.Fatalf("store failure response = %d %s", response.Code, response.Body.String())
	}
}
