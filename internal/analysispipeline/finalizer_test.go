package analysispipeline_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/analysispipeline"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

func TestFinalizerPreservesStageTransactionsAndClaimFencing(t *testing.T) {
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "finalizer.db")+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
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
	repositories := repo.NewStore(db)
	if err := repositories.Create(ctx, &repo.Repository{ID: "finalizer-repo", UserID: "owner", Name: "finalizer", GitURL: "https://github.com/example/finalizer"}); err != nil {
		t.Fatal(err)
	}
	snapshots := snapshot.NewStore(db)
	codeintel := codeintelstore.NewStore(db)
	finalizer := analysispipeline.NewFinalizer(snapshots, codeintel)
	jobStore := jobs.NewStoreWithDriver(sqlDB, "sqlite3")
	prepared, err := analysispipeline.NewService(analysispipeline.NewStore(db)).Prepare(ctx, analysispipeline.PrepareSpec{
		RepositoryID: "finalizer-repo", SourceRef: "main", CommitSHA: "0123456789012345678901234567890123456789",
		PipelineVersion: revision.PipelineVersion, PipelineFingerprint: revision.ComputePipelineFingerprint(),
		SnapshotBasePath: t.TempDir(), ModulePath: "finalizer",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The existing finalizers propagate this generation to the next stage.
	if err := db.Model(&revision.AnalysisRevision{}).Where("id = ?", prepared.ID).Update("execution_generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeMaterializeSnapshot, prepared.SnapshotID).Update("execution_generation", 2).Error; err != nil {
		t.Fatal(err)
	}

	snapshotJob := claimFinalizerJob(t, ctx, jobStore, jobs.JobTypeMaterializeSnapshot)
	snapshotResult := analysispipeline.SnapshotStageResult{
		Ownership: ownershipForFinalizerJob(snapshotJob), RevisionID: prepared.ID, SnapshotID: prepared.SnapshotID,
		MaterializedPath: filepath.Join(t.TempDir(), "source"), ModulePath: "finalizer",
		CommitSHA: prepared.CommitSHA, ContentHash: "content-hash", FileCount: 3, TotalBytes: 42, ReadyAt: time.Now().UTC(),
	}
	staleSnapshot := snapshotResult
	staleSnapshot.Ownership.ClaimToken += "-stale"
	if err := finalizer.FinalizeSnapshot(ctx, staleSnapshot); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale snapshot claim = %v, want ownership lost", err)
	}
	assertFinalizerRevision(t, ctx, db, prepared.ID, revision.StatusPreparing, revision.StageMaterializing, 0, 0)
	assertFinalizerJob(t, ctx, jobStore, snapshotJob.ID, jobs.StatusRunning, 2)
	if err := db.Exec(`CREATE TRIGGER reject_snapshot_stage BEFORE UPDATE ON analysis_revisions WHEN NEW.stage = 'BUILDING_CODE_INDEX' BEGIN SELECT RAISE(FAIL, 'injected snapshot stage failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := finalizer.FinalizeSnapshot(ctx, snapshotResult); err == nil {
		t.Fatal("snapshot stage unexpectedly committed across injected revision failure")
	}
	if err := db.Exec(`DROP TRIGGER reject_snapshot_stage`).Error; err != nil {
		t.Fatal(err)
	}
	rollbackSnap, err := snapshots.GetByID(ctx, prepared.SnapshotID)
	if err != nil || rollbackSnap.Status != snapshot.StatusMaterializing {
		t.Fatalf("snapshot stage rollback = %+v err=%v", rollbackSnap, err)
	}
	assertFinalizerRevision(t, ctx, db, prepared.ID, revision.StatusPreparing, revision.StageMaterializing, 0, 0)
	assertFinalizerJob(t, ctx, jobStore, snapshotJob.ID, jobs.StatusRunning, 2)
	var prematureCodeBuilds int64
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("analysis_revision_id = ?", prepared.ID).Count(&prematureCodeBuilds).Error; err != nil || prematureCodeBuilds != 0 {
		t.Fatalf("snapshot rollback left %d code builds, err=%v", prematureCodeBuilds, err)
	}
	if err := finalizer.FinalizeSnapshot(ctx, snapshotResult); err != nil {
		t.Fatal(err)
	}
	snap, err := snapshots.GetByID(ctx, prepared.SnapshotID)
	if err != nil || snap.Status != snapshot.StatusReady || snap.ContentHash != snapshotResult.ContentHash || snap.FileCount != 3 || snap.TotalBytes != 42 || snap.MaterializedPath != snapshotResult.MaterializedPath {
		t.Fatalf("snapshot stage result = %+v err=%v", snap, err)
	}
	assertFinalizerJob(t, ctx, jobStore, snapshotJob.ID, jobs.StatusSucceeded, 2)
	rev := assertFinalizerRevision(t, ctx, db, prepared.ID, revision.StatusPreparing, revision.StageBuildingCode, 1, 0)
	codeBuild, err := codeintel.GetByID(ctx, rev.CodeIndexBuildID)
	if err != nil || codeBuild.AnalysisRevisionID != prepared.ID || codeBuild.SnapshotID != snap.ID || codeBuild.ModulePath != "finalizer" {
		t.Fatalf("created code index = %+v err=%v", codeBuild, err)
	}

	codeJob := claimFinalizerJob(t, ctx, jobStore, jobs.JobTypeBuildCodeIndex)
	if codeJob.ExecutionGeneration != 2 {
		t.Fatalf("code-index job generation = %d, want 2", codeJob.ExecutionGeneration)
	}
	if err := codeintel.MarkBuildBuilding(ctx, codeBuild.ID); err != nil {
		t.Fatal(err)
	}
	codeResult := analysispipeline.CodeIndexStageResult{
		Ownership: ownershipForFinalizerJob(codeJob), RevisionID: prepared.ID, CodeIndexBuildID: codeBuild.ID,
		AnalysisResult: &codeintelmodel.AnalysisResult{ModulePath: "finalizer", BuildContext: codeintelmodel.DefaultBuildContext()},
	}
	staleCode := codeResult
	staleCode.Ownership.ClaimToken += "-stale"
	if err := finalizer.FinalizeCodeIndex(ctx, staleCode); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale code-index claim = %v, want ownership lost", err)
	}
	assertFinalizerRevision(t, ctx, db, prepared.ID, revision.StatusPreparing, revision.StageBuildingCode, 1, 0)
	assertFinalizerJob(t, ctx, jobStore, codeJob.ID, jobs.StatusRunning, 2)
	if err := db.Exec(`CREATE TRIGGER reject_code_stage BEFORE UPDATE ON analysis_revisions WHEN NEW.stage = 'BUILDING_RETRIEVAL' BEGIN SELECT RAISE(FAIL, 'injected code stage failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := finalizer.FinalizeCodeIndex(ctx, codeResult); err == nil {
		t.Fatal("code-index stage unexpectedly committed across injected revision failure")
	}
	if err := db.Exec(`DROP TRIGGER reject_code_stage`).Error; err != nil {
		t.Fatal(err)
	}
	rolledBackCode, err := codeintel.GetByID(ctx, codeBuild.ID)
	if err != nil || rolledBackCode.Status != codeintelmodel.BuildStatusBuilding {
		t.Fatalf("code-index stage rollback = %+v err=%v", rolledBackCode, err)
	}
	assertFinalizerRevision(t, ctx, db, prepared.ID, revision.StatusPreparing, revision.StageBuildingCode, 1, 0)
	assertFinalizerJob(t, ctx, jobStore, codeJob.ID, jobs.StatusRunning, 2)
	var prematureRetrievalBuilds int64
	if err := db.Model(&codeintelmodel.RetrievalBuild{}).Where("analysis_revision_id = ?", prepared.ID).Count(&prematureRetrievalBuilds).Error; err != nil || prematureRetrievalBuilds != 0 {
		t.Fatalf("code-index rollback left %d retrieval builds, err=%v", prematureRetrievalBuilds, err)
	}
	if err := finalizer.FinalizeCodeIndex(ctx, codeResult); err != nil {
		t.Fatal(err)
	}
	assertFinalizerJob(t, ctx, jobStore, codeJob.ID, jobs.StatusSucceeded, 2)
	rev = assertFinalizerRevision(t, ctx, db, prepared.ID, revision.StatusPreparing, revision.StageBuildingSearch, 1, 1)
	codeBuild, err = codeintel.GetByID(ctx, codeBuild.ID)
	if err != nil || codeBuild.Status != codeintelmodel.BuildStatusReady {
		t.Fatalf("code-index stage result = %+v err=%v", codeBuild, err)
	}
	retrievalBuild, err := codeintel.GetRetrievalBuildByID(ctx, rev.RetrievalBuildID)
	if err != nil || retrievalBuild.AnalysisRevisionID != prepared.ID || retrievalBuild.CodeIndexBuildID != codeBuild.ID {
		t.Fatalf("created retrieval build = %+v err=%v", retrievalBuild, err)
	}

	retrievalJob := claimFinalizerJob(t, ctx, jobStore, jobs.JobTypeBuildRetrieval)
	if retrievalJob.ExecutionGeneration != 2 {
		t.Fatalf("retrieval job generation = %d, want 2", retrievalJob.ExecutionGeneration)
	}
	if err := codeintel.MarkRetrievalBuilding(ctx, retrievalBuild.ID); err != nil {
		t.Fatal(err)
	}
	retrievalResult := analysispipeline.RetrievalStageResult{
		Ownership: ownershipForFinalizerJob(retrievalJob), RevisionID: prepared.ID, RetrievalBuildID: retrievalBuild.ID,
		ArtifactPath: filepath.Join(t.TempDir(), "index.json"), ArtifactHash: "artifact-hash", DocCount: 7,
	}
	staleRetrieval := retrievalResult
	staleRetrieval.Ownership.ClaimToken += "-stale"
	if err := finalizer.FinalizeRetrieval(ctx, staleRetrieval); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale retrieval claim = %v, want ownership lost", err)
	}
	assertFinalizerRevision(t, ctx, db, prepared.ID, revision.StatusPreparing, revision.StageBuildingSearch, 1, 1)
	assertFinalizerJob(t, ctx, jobStore, retrievalJob.ID, jobs.StatusRunning, 2)
	if err := db.Exec(`CREATE TRIGGER reject_retrieval_stage BEFORE UPDATE ON analysis_revisions WHEN NEW.status = 'READY' BEGIN SELECT RAISE(FAIL, 'injected retrieval stage failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := finalizer.FinalizeRetrieval(ctx, retrievalResult); err == nil {
		t.Fatal("retrieval stage unexpectedly committed across injected revision failure")
	}
	if err := db.Exec(`DROP TRIGGER reject_retrieval_stage`).Error; err != nil {
		t.Fatal(err)
	}
	rolledBackRetrieval, err := codeintel.GetRetrievalBuildByID(ctx, retrievalBuild.ID)
	if err != nil || rolledBackRetrieval.Status != codeintelmodel.BuildStatusBuilding {
		t.Fatalf("retrieval stage rollback = %+v err=%v", rolledBackRetrieval, err)
	}
	assertFinalizerRevision(t, ctx, db, prepared.ID, revision.StatusPreparing, revision.StageBuildingSearch, 1, 1)
	assertFinalizerJob(t, ctx, jobStore, retrievalJob.ID, jobs.StatusRunning, 2)
	if err := finalizer.FinalizeRetrieval(ctx, retrievalResult); err != nil {
		t.Fatal(err)
	}
	assertFinalizerJob(t, ctx, jobStore, retrievalJob.ID, jobs.StatusSucceeded, 2)
	assertFinalizerRevision(t, ctx, db, prepared.ID, revision.StatusReady, revision.StageReady, 1, 1)
	retrievalBuild, err = codeintel.GetRetrievalBuildByID(ctx, retrievalBuild.ID)
	if err != nil || retrievalBuild.Status != codeintelmodel.BuildStatusReady || retrievalBuild.ArtifactPath != retrievalResult.ArtifactPath || retrievalBuild.ArtifactHash != retrievalResult.ArtifactHash || retrievalBuild.DocumentCount != 7 {
		t.Fatalf("retrieval stage result = %+v err=%v", retrievalBuild, err)
	}
}

func claimFinalizerJob(t *testing.T, ctx context.Context, store *jobs.Store, jobType jobs.JobType) *jobs.AnalysisJob {
	t.Helper()
	claimed, err := store.ClaimJobs(ctx, "finalizer-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].JobType != jobType {
		t.Fatalf("claim %s = %+v err=%v", jobType, claimed, err)
	}
	job := claimed[0]
	if _, err := store.MarkExecutionStarted(ctx, job.ID, *job.WorkerID, *job.ClaimToken, job.ExecutionGeneration); err != nil {
		t.Fatal(err)
	}
	return job
}

func ownershipForFinalizerJob(job *jobs.AnalysisJob) analysispipeline.JobOwnership {
	return analysispipeline.JobOwnership{JobID: job.ID, WorkerID: *job.WorkerID, ClaimToken: *job.ClaimToken}
}

func assertFinalizerRevision(t *testing.T, ctx context.Context, db *gorm.DB, id string, status revision.Status, stage revision.Stage, wantCode, wantRetrieval int) *revision.AnalysisRevision {
	t.Helper()
	rev, err := revision.NewStore(db).GetByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Status != status || rev.Stage != stage || (rev.CodeIndexBuildID > 0) != (wantCode > 0) || (rev.RetrievalBuildID > 0) != (wantRetrieval > 0) || rev.ExecutionGeneration != 2 {
		t.Fatalf("revision stage = %+v, want %s/%s code=%d retrieval=%d generation=2", rev, status, stage, wantCode, wantRetrieval)
	}
	return rev
}

func assertFinalizerJob(t *testing.T, ctx context.Context, store *jobs.Store, id int64, status jobs.JobStatus, generation int) {
	t.Helper()
	job, err := store.GetJobByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != status || job.ExecutionGeneration != generation {
		t.Fatalf("job outcome = %+v, want %s generation %d", job, status, generation)
	}
}
