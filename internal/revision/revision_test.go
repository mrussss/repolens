package revision_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
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
	if first.Status != revision.StatusPreparing || first.Stage != revision.StageMaterializing || first.SnapshotID == "" || first.CodeIndexBuildID != 0 || first.RetrievalBuildID != 0 {
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
	if jobCount != 1 {
		t.Fatalf("job count = %d, want only the current preparation stage job", jobCount)
	}

	snap, err := snapshot.NewStore(db).GetByID(ctx, first.SnapshotID)
	if err != nil || snap.AnalysisRevisionID != first.ID {
		t.Fatalf("snapshot lineage = %+v err=%v", snap, err)
	}
}

func TestPrepareCreatesNewSnapshotForExistingCommitAfterPipelineChange(t *testing.T) {
	db := newRevisionDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	repository := &repo.Repository{ID: "repo-pipeline-upgrade", UserID: "local-user", Name: "fixture", GitURL: "https://github.com/example/pipeline-upgrade", DefaultRef: "main"}
	if err := repoStore.Create(ctx, repository); err != nil {
		t.Fatal(err)
	}
	const commitSHA = "0123456789012345678901234567890123456789"
	oldRevision := &revision.AnalysisRevision{
		ID: "legacy-v21-revision", RepositoryID: repository.ID, SourceRef: "main", CommitSHA: commitSHA,
		PipelineVersion: "v2.1.0", PipelineFingerprint: "legacy-v2.1-fingerprint",
		Status: revision.StatusReady, Stage: revision.StageReady,
	}
	if err := db.Create(oldRevision).Error; err != nil {
		t.Fatalf("seed historical READY revision: %v", err)
	}
	oldSnapshot := &snapshot.RepositorySnapshot{
		ID: "legacy-v21-snapshot", RepositoryID: repository.ID, AnalysisRevisionID: oldRevision.ID,
		CommitSHA: commitSHA, Ref: "main", MaterializedPath: filepath.Join(t.TempDir(), "legacy-source"),
		ContentHash: "legacy-content-hash", Status: snapshot.StatusReady,
	}
	if err := db.Create(oldSnapshot).Error; err != nil {
		t.Fatalf("seed historical READY snapshot: %v", err)
	}

	service := revision.NewService(revision.NewStore(db), repoStore, fixedResolver{sha: commitSHA}, t.TempDir())
	current, created, err := service.Prepare(ctx, "local-user", repository.ID, "main")
	if err != nil || !created {
		t.Fatalf("prepare after pipeline change = created=%v err=%v", created, err)
	}
	if current.ID == oldRevision.ID || current.SnapshotID == oldSnapshot.ID || current.PipelineFingerprint == oldRevision.PipelineFingerprint {
		t.Fatalf("new pipeline reused historical revision identity: current=%+v old=%+v", current, oldRevision)
	}
	var snapshots []snapshot.RepositorySnapshot
	if err := db.Where("repository_id = ? AND commit_sha = ?", repository.ID, commitSHA).Find(&snapshots).Error; err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 {
		t.Fatalf("snapshot count for same commit across revisions = %d, want 2", len(snapshots))
	}
	if snapshots[0].AnalysisRevisionID == snapshots[1].AnalysisRevisionID {
		t.Fatalf("revision-scoped snapshots have duplicate lineage IDs: %+v", snapshots)
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
	bc := codeintelmodel.DefaultBuildContext()
	codeBuild := &codeintelmodel.CodeIndexBuild{
		SnapshotID: value.SnapshotID, AnalysisRevisionID: value.ID,
		ParserVersion: codeintelmodel.CurrentParserVersion, AnalyzerVersion: codeintelmodel.CurrentAnalyzerVersion,
		SymbolSchemaVersion: codeintelmodel.CurrentSymbolSchemaVersion, BuildContextHash: bc.BuildContextHash(),
		ModulePath: "state", GOOS: bc.GOOS, GOARCH: bc.GOARCH, BuildTagsHash: bc.BuildTagsHash(),
		Status: codeintelmodel.BuildStatusReady,
	}
	if err := db.Create(codeBuild).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&revision.AnalysisRevision{}).Where("id = ?", value.ID).Update("code_index_build_id", codeBuild.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.MarkCodeIndexReady(ctx, value.ID, codeBuild.ID); err != nil {
		t.Fatal(err)
	}
	retrievalBuild := &codeintelmodel.RetrievalBuild{
		CodeIndexBuildID: codeBuild.ID, AnalysisRevisionID: value.ID, Strategy: "BM25",
		RetrievalVersion: codeintelmodel.CurrentRetrievalVersion, TokenizerVersion: codeintelmodel.CurrentTokenizerVersion,
		ConfigHash: "config-v2.2", Status: codeintelmodel.BuildStatusReady,
	}
	if err := db.Create(retrievalBuild).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&revision.AnalysisRevision{}).Where("id = ?", value.ID).Update("retrieval_build_id", retrievalBuild.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.MarkRetrievalReady(ctx, value.ID, retrievalBuild.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := store.GetByID(ctx, value.ID)
	if err != nil || ready.Status != revision.StatusReady || ready.Stage != revision.StageReady {
		t.Fatalf("ready revision = %+v err=%v", ready, err)
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

func TestConcurrentRevisionRetryHasSingleGenerationWinner(t *testing.T) {
	db := newRevisionDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{
		ID: "repo-retry-cas", UserID: "user-retry-cas", Name: "retry-cas",
		GitURL: "https://github.com/example/retry-cas", DefaultRef: "main",
	}); err != nil {
		t.Fatal(err)
	}
	store := revision.NewStore(db)
	service := revision.NewService(store, repoStore, fixedResolver{sha: "0123456789012345678901234567890123456789"}, t.TempDir())
	value, created, err := service.Prepare(ctx, "user-retry-cas", "repo-retry-cas", "main")
	if err != nil || !created {
		t.Fatalf("Prepare = created %t err %v", created, err)
	}
	if err := db.Model(&revision.AnalysisRevision{}).Where("id = ?", value.ID).Updates(map[string]interface{}{
		"status": revision.StatusFailed, "stage": revision.StageMaterializing, "execution_generation": 1,
	}).Error; err != nil {
		t.Fatal(err)
	}

	const callers = 50
	start := make(chan struct{})
	results := make(chan error, callers)
	var workers sync.WaitGroup
	workers.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer workers.Done()
			<-start
			_, retryErr := store.Retry(ctx, value.ID)
			results <- retryErr
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	succeeded := 0
	conflicts := 0
	for retryErr := range results {
		if retryErr == nil {
			succeeded++
		} else if errors.Is(retryErr, revision.ErrRetryConflict) {
			conflicts++
		} else {
			t.Errorf("Retry returned unexpected error: %v", retryErr)
		}
	}
	if succeeded != 1 || conflicts != callers-1 {
		t.Fatalf("concurrent Retry results = %d successes, %d conflicts; want 1 success and %d conflicts", succeeded, conflicts, callers-1)
	}

	saved, err := store.GetByID(ctx, value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != revision.StatusPreparing || saved.ExecutionGeneration != 2 || saved.Version != value.Version+1 {
		t.Fatalf("revision after concurrent Retry = status %s generation %d version %d", saved.Status, saved.ExecutionGeneration, saved.Version)
	}
	var jobCount int64
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeMaterializeSnapshot, value.SnapshotID).Count(&jobCount).Error; err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 {
		t.Fatalf("snapshot jobs after concurrent Retry = %d, want exactly one", jobCount)
	}
	var job jobs.AnalysisJob
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeMaterializeSnapshot, value.SnapshotID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusPending || job.ExecutionGeneration != 2 || job.AttemptCount != 0 {
		t.Fatalf("snapshot job after concurrent Retry = status %s generation %d attempt %d", job.Status, job.ExecutionGeneration, job.AttemptCount)
	}
}

func TestRevisionRetryCASConflictDoesNotResetPipelineResources(t *testing.T) {
	db := newRevisionDB(t)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{
		ID: "repo-retry-cas-conflict", UserID: "user-retry-cas-conflict", Name: "retry-cas-conflict",
		GitURL: "https://github.com/example/retry-cas-conflict", DefaultRef: "main",
	}); err != nil {
		t.Fatal(err)
	}
	store := revision.NewStore(db)
	service := revision.NewService(store, repoStore, fixedResolver{sha: "abcdef0123456789abcdef0123456789abcdef01"}, t.TempDir())
	value, created, err := service.Prepare(ctx, "user-retry-cas-conflict", "repo-retry-cas-conflict", "main")
	if err != nil || !created {
		t.Fatalf("Prepare = created %t err %v", created, err)
	}
	if err := db.Model(&revision.AnalysisRevision{}).Where("id = ?", value.ID).Update("status", revision.StatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&snapshot.RepositorySnapshot{}).Where("id = ?", value.SnapshotID).Update("status", snapshot.StatusReady).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER ignore_revision_retry BEFORE UPDATE ON analysis_revisions WHEN OLD.status = 'FAILED' BEGIN SELECT RAISE(IGNORE); END`).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER ignore_revision_retry")
	if _, err := store.Retry(ctx, value.ID); !errors.Is(err, revision.ErrRetryConflict) {
		t.Fatalf("Retry error = %v, want ErrRetryConflict", err)
	}
	saved, err := store.GetByID(ctx, value.ID)
	if err != nil || saved.Status != revision.StatusFailed || saved.ExecutionGeneration != 1 {
		t.Fatalf("revision after rejected CAS = %+v err=%v", saved, err)
	}
	snap, err := snapshot.NewStore(db).GetByID(ctx, value.SnapshotID)
	if err != nil || snap.Status != snapshot.StatusReady {
		t.Fatalf("snapshot was reset before CAS ownership: %+v err=%v", snap, err)
	}
	var buildCount int64
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("snapshot_id = ?", value.SnapshotID).Count(&buildCount).Error; err != nil {
		t.Fatal(err)
	}
	if buildCount != 0 {
		t.Fatalf("CAS loser created %d CodeIndex builds, want none", buildCount)
	}
	var job jobs.AnalysisJob
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeMaterializeSnapshot, value.SnapshotID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusPending || job.ExecutionGeneration != 1 || job.AttemptCount != 0 {
		t.Fatalf("job reset before CAS ownership: %+v", job)
	}
}
