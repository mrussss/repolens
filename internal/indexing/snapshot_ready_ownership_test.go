package indexing_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/analysispipeline"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/indexing"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

type readyOwnershipFixture struct {
	db        *gorm.DB
	jobs      *jobs.Store
	snapshots *snapshot.GormStore
	revisions *revision.GormStore
	handler   *indexing.SnapshotJobHandler
}

const (
	readyOwnerRevisionID = "revision-ready-owner"
	readyOwnerSnapshotID = "snapshot-ready-owner"
	readyOwnerRepoID     = "repository-ready-owner"
)

func newReadyOwnershipFixture(t *testing.T) readyOwnershipFixture {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "ready_ownership.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	if err := db.Create(&revision.AnalysisRevision{
		ID: readyOwnerRevisionID, RepositoryID: readyOwnerRepoID, SourceRef: "main",
		CommitSHA: commitSHA, PipelineVersion: revision.PipelineVersion,
		PipelineFingerprint: revision.ComputePipelineFingerprint(), SnapshotID: readyOwnerSnapshotID,
		CodeIndexBuildID: 11, RetrievalBuildID: 22,
		Status: revision.StatusPreparing, Stage: revision.StageBuildingSearch,
		ExecutionGeneration: 2, Version: 7,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&snapshot.RepositorySnapshot{
		ID: readyOwnerSnapshotID, AnalysisRevisionID: readyOwnerRevisionID,
		RepositoryID: readyOwnerRepoID, CommitSHA: commitSHA, Ref: "main", Status: snapshot.StatusReady,
	}).Error; err != nil {
		t.Fatal(err)
	}
	snapshots := snapshot.NewStore(db)
	revisions := revision.NewStore(db)
	handler := indexing.NewSnapshotJobHandler(nil, snapshots, nil, nil, nil, nil, nil, nil).
		WithRevisionStore(revisions).
		WithFinalizer(analysispipeline.NewFinalizer(snapshots, codeintelstore.NewStore(db)))
	return readyOwnershipFixture{db: db, jobs: jobs.NewStoreWithDriver(sqlDB, "sqlite3"), snapshots: snapshots, revisions: revisions, handler: handler}
}

func assertReadyOwnershipUnchanged(t *testing.T, fixture readyOwnershipFixture) {
	t.Helper()
	rev, err := fixture.revisions.GetByID(context.Background(), readyOwnerRevisionID)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Status != revision.StatusPreparing || rev.Stage != revision.StageBuildingSearch || rev.Version != 7 ||
		rev.ExecutionGeneration != 2 || rev.CodeIndexBuildID != 11 || rev.RetrievalBuildID != 22 {
		t.Fatalf("READY replay changed advanced revision: status=%s stage=%s version=%d generation=%d code_index=%d retrieval=%d",
			rev.Status, rev.Stage, rev.Version, rev.ExecutionGeneration, rev.CodeIndexBuildID, rev.RetrievalBuildID)
	}
	var codeBuilds, retrievalBuilds, nextJobs int64
	if err := fixture.db.Model(&codeintelmodel.CodeIndexBuild{}).Count(&codeBuilds).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&codeintelmodel.RetrievalBuild{}).Count(&retrievalBuilds).Error; err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&jobs.AnalysisJob{}).Where("job_type IN ?", []jobs.JobType{jobs.JobTypeBuildCodeIndex, jobs.JobTypeBuildRetrieval}).Count(&nextJobs).Error; err != nil {
		t.Fatal(err)
	}
	if codeBuilds != 0 || retrievalBuilds != 0 || nextJobs != 0 {
		t.Fatalf("READY replay created next-stage state: code=%d retrieval=%d jobs=%d", codeBuilds, retrievalBuilds, nextJobs)
	}
}

func TestReadySnapshotReplayDoesNotRegressAdvancedRevision(t *testing.T) {
	fixture := newReadyOwnershipFixture(t)
	if err := fixture.handler.Execute(context.Background(), &jobs.AnalysisJob{ID: 42, ResourceID: readyOwnerSnapshotID}); err != nil {
		t.Fatal(err)
	}
	assertReadyOwnershipUnchanged(t, fixture)
}

func TestReadySnapshotStaleClaimCannotChangeAdvancedRevision(t *testing.T) {
	fixture := newReadyOwnershipFixture(t)
	ctx := context.Background()
	job := &jobs.AnalysisJob{JobType: jobs.JobTypeMaterializeSnapshot, ResourceID: readyOwnerSnapshotID, MaxAttempts: 3}
	if err := fixture.jobs.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	oldClaim, err := fixture.jobs.ClaimJobs(ctx, "old-owner", 1, time.Minute)
	if err != nil || len(oldClaim) != 1 {
		t.Fatalf("old claim = %+v, err=%v", oldClaim, err)
	}
	if _, err := fixture.jobs.MarkExecutionStarted(ctx, oldClaim[0].ID, "old-owner", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration); err != nil {
		t.Fatal(err)
	}
	if err := fixture.db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
		"status": jobs.StatusPending, "execution_generation": 2, "worker_id": nil,
		"claim_token": nil, "lease_until": nil, "execution_started": false, "next_run_at": time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	newClaim, err := fixture.jobs.ClaimJobs(ctx, "new-owner", 1, time.Minute)
	if err != nil || len(newClaim) != 1 || newClaim[0].ExecutionGeneration != 2 {
		t.Fatalf("replacement claim = %+v, err=%v", newClaim, err)
	}
	if err := fixture.handler.Execute(ctx, oldClaim[0]); err != nil {
		t.Fatal(err)
	}
	assertReadyOwnershipUnchanged(t, fixture)
	currentJob, err := fixture.jobs.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if currentJob.Status != jobs.StatusRunning || currentJob.ExecutionGeneration != 2 || currentJob.WorkerID == nil || *currentJob.WorkerID != "new-owner" ||
		currentJob.ClaimToken == nil || *currentJob.ClaimToken != *newClaim[0].ClaimToken {
		t.Fatalf("READY replay changed replacement ownership: %+v", currentJob)
	}
}

type readyRevisionRecorder struct{ calls int }

func (r *readyRevisionRecorder) MarkSnapshotReady(context.Context, string, string) error {
	r.calls++
	return nil
}

func TestReadySnapshotFinalizerWinsOverLegacyRevisionStore(t *testing.T) {
	legacy := &readyRevisionRecorder{}
	store := &materializationRecorder{snap: &snapshot.RepositorySnapshot{ID: "ready", AnalysisRevisionID: "revision", Status: snapshot.StatusReady}}
	handler := indexing.NewSnapshotJobHandler(nil, store, nil, nil, nil, nil, nil, nil).
		WithRevisionStore(legacy).
		WithFinalizer(analysispipeline.NewFinalizer(nil, nil))
	if err := handler.Execute(context.Background(), &jobs.AnalysisJob{ResourceID: "ready"}); err != nil {
		t.Fatal(err)
	}
	if legacy.calls != 0 {
		t.Fatalf("production READY path called legacy revision store %d times", legacy.calls)
	}
}

func TestReadySnapshotLegacyCompatibilityWithoutFinalizer(t *testing.T) {
	legacy := &readyRevisionRecorder{}
	store := &materializationRecorder{snap: &snapshot.RepositorySnapshot{ID: "ready", AnalysisRevisionID: "revision", Status: snapshot.StatusReady}}
	handler := indexing.NewSnapshotJobHandler(nil, store, nil, nil, nil, nil, nil, nil).WithRevisionStore(legacy)
	if err := handler.Execute(context.Background(), &jobs.AnalysisJob{ResourceID: "ready"}); err != nil {
		t.Fatal(err)
	}
	if legacy.calls != 1 {
		t.Fatalf("legacy READY path calls = %d, want 1", legacy.calls)
	}
}
