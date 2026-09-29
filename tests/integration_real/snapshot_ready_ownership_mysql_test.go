package integration_real

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"repolens/internal/analysispipeline"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/indexing"
	"repolens/internal/jobs"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

func TestRealMySQL_SnapshotReadyStaleClaimCannotRegressRevision(t *testing.T) {
	db, jobStore, cleanup := setupRealMySQL(t)
	defer cleanup()
	ctx := context.Background()
	const repositoryID = "ready-ownership-mysql"
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	if err := repo.NewStore(db).Create(ctx, &repo.Repository{
		ID: repositoryID, UserID: "owner", Name: "ready-ownership", GitURL: "https://github.com/example/ready-ownership",
	}); err != nil {
		t.Fatal(err)
	}
	prepared, err := analysispipeline.NewService(analysispipeline.NewStore(db)).Prepare(ctx, analysispipeline.PrepareSpec{
		RepositoryID: repositoryID, SourceRef: "main", CommitSHA: commitSHA,
		PipelineVersion: revision.PipelineVersion, PipelineFingerprint: revision.ComputePipelineFingerprint(),
		SnapshotBasePath: t.TempDir(), ModulePath: "ready-ownership",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshots := snapshot.NewStore(db)
	codeIntel := codeintelstore.NewStore(db)
	revisions := revision.NewStore(db)
	finalizer := analysispipeline.NewFinalizer(snapshots, codeIntel)
	claims, err := jobStore.ClaimJobs(ctx, "old-snapshot-owner", 1, time.Minute)
	if err != nil || len(claims) != 1 {
		t.Fatalf("snapshot claim = %+v, err=%v", claims, err)
	}
	oldClaim := claims[0]
	if _, err := jobStore.MarkExecutionStarted(ctx, oldClaim.ID, "old-snapshot-owner", *oldClaim.ClaimToken, oldClaim.ExecutionGeneration); err != nil {
		t.Fatal(err)
	}
	result := analysispipeline.SnapshotStageResult{
		Ownership:  analysispipeline.JobOwnership{JobID: oldClaim.ID, WorkerID: "old-snapshot-owner", ClaimToken: *oldClaim.ClaimToken},
		RevisionID: prepared.ID, SnapshotID: prepared.SnapshotID,
		MaterializedPath: "/tmp/ready-ownership-source", ModulePath: "ready-ownership", CommitSHA: commitSHA,
		ContentHash: "ready-ownership-content", FileCount: 1, TotalBytes: 12, ReadyAt: time.Now().UTC(),
	}
	stale := result
	stale.Ownership.ClaimToken += "-stale"
	if err := finalizer.FinalizeSnapshot(ctx, stale); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale snapshot finalization = %v, want ownership lost", err)
	}
	snap, err := snapshots.GetByID(ctx, prepared.SnapshotID)
	if err != nil || snap.Status != snapshot.StatusMaterializing {
		t.Fatalf("stale finalization changed snapshot: %+v err=%v", snap, err)
	}
	rev, err := revisions.GetByID(ctx, prepared.ID)
	if err != nil || rev.Stage != revision.StageMaterializing || rev.CodeIndexBuildID != 0 {
		t.Fatalf("stale finalization changed revision: %+v err=%v", rev, err)
	}
	job, err := jobStore.GetJobByID(ctx, oldClaim.ID)
	if err != nil || job.Status != jobs.StatusRunning {
		t.Fatalf("stale finalization changed job: %+v err=%v", job, err)
	}
	if err := finalizer.FinalizeSnapshot(ctx, result); err != nil {
		t.Fatal(err)
	}
	snap, err = snapshots.GetByID(ctx, prepared.SnapshotID)
	if err != nil || snap.Status != snapshot.StatusReady {
		t.Fatalf("snapshot success was not atomic: %+v err=%v", snap, err)
	}
	rev, err = revisions.GetByID(ctx, prepared.ID)
	if err != nil || rev.Stage != revision.StageBuildingCode || rev.CodeIndexBuildID == 0 {
		t.Fatalf("snapshot success did not advance revision: %+v err=%v", rev, err)
	}
	build, err := codeIntel.GetByID(ctx, rev.CodeIndexBuildID)
	if err != nil || build.AnalysisRevisionID != prepared.ID || build.SnapshotID != prepared.SnapshotID {
		t.Fatalf("snapshot success did not create code index: %+v err=%v", build, err)
	}
	var nextJob jobs.AnalysisJob
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildCodeIndex, fmt.Sprint(build.ID)).First(&nextJob).Error; err != nil {
		t.Fatalf("snapshot success did not create next-stage job: %v", err)
	}
	job, err = jobStore.GetJobByID(ctx, oldClaim.ID)
	if err != nil || job.Status != jobs.StatusSucceeded {
		t.Fatalf("snapshot success did not finalize job: %+v err=%v", job, err)
	}
	if err := db.Model(&revision.AnalysisRevision{}).Where("id = ?", prepared.ID).Updates(map[string]interface{}{
		"stage": revision.StageBuildingSearch, "version": 9, "retrieval_build_id": 77,
	}).Error; err != nil {
		t.Fatal(err)
	}
	handler := indexing.NewSnapshotJobHandler(nil, snapshots, nil, nil, nil, nil, nil, nil).
		WithRevisionStore(revisions).WithFinalizer(finalizer)
	if err := handler.Execute(ctx, oldClaim); err != nil {
		t.Fatal(err)
	}
	rev, err = revisions.GetByID(ctx, prepared.ID)
	if err != nil || rev.Stage != revision.StageBuildingSearch || rev.Version != 9 || rev.CodeIndexBuildID != build.ID || rev.RetrievalBuildID != 77 {
		t.Fatalf("stale READY replay regressed revision: %+v err=%v", rev, err)
	}
	var codeBuildCount, nextJobCount int64
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("analysis_revision_id = ?", prepared.ID).Count(&codeBuildCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ?", jobs.JobTypeBuildCodeIndex).Count(&nextJobCount).Error; err != nil {
		t.Fatal(err)
	}
	if codeBuildCount != 1 || nextJobCount != 1 {
		t.Fatalf("stale READY replay changed next-stage state: builds=%d jobs=%d", codeBuildCount, nextJobCount)
	}
	job, err = jobStore.GetJobByID(ctx, oldClaim.ID)
	if err != nil || job.Status != jobs.StatusSucceeded {
		t.Fatalf("stale READY replay changed terminal job: %+v err=%v", job, err)
	}
}
