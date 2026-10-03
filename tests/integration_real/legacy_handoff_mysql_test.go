package integration_real

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/indexing"
	"repolens/internal/jobs"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/snapshot"
)

func TestRealMySQL_LegacySnapshotHandoffIsAtomic(t *testing.T) {
	if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "" {
		t.Skip("skipping real MySQL legacy handoff test (set REPOLENS_REQUIRE_REAL_INTEGRATION=1)")
	}
	db, jobsStore, cleanup := setupRealMySQL(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	const (
		repoID     = "repo-mysql-legacy-snapshot-atomic"
		snapshotID = "snap-mysql-legacy-snapshot-atomic"
		commitSHA  = "0123456789abcdef0123456789abcdef01234567"
		trigger    = "fail_legacy_snapshot_child_job"
	)
	t.Cleanup(func() { _ = db.Exec("DROP TRIGGER IF EXISTS " + trigger).Error })
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{
		ID: repoID, UserID: "user-mysql-legacy", Name: "example.com/mysql-legacy",
		GitURL: "https://github.com/example/mysql-legacy", DefaultRef: "main", Status: repo.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	snapshotStore := snapshot.NewStore(db)
	if err := snapshotStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapshotID, RepositoryID: repoID, Ref: "main", CommitSHA: commitSHA, Status: snapshot.StatusMaterializing,
	}); err != nil {
		t.Fatal(err)
	}
	ciStore := codeintelstore.NewStore(db)
	childBuild, created, err := ciStore.GetOrCreateBuild(ctx, snapshotID, "example.com/mysql-legacy", codeintelmodel.DefaultBuildContext())
	if err != nil || !created {
		t.Fatalf("pre-create CodeIndexBuild: build=%+v created=%t err=%v", childBuild, created, err)
	}
	childJobResource := fmt.Sprintf("%d", childBuild.ID)
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildCodeIndex, childJobResource).Delete(&jobs.AnalysisJob{}).Error; err != nil {
		t.Fatalf("remove pre-created CodeIndex Job: %v", err)
	}
	parentJob := &jobs.AnalysisJob{JobType: jobs.JobTypeMaterializeSnapshot, ResourceID: snapshotID, MaxAttempts: 3}
	if err := jobsStore.CreateJob(ctx, parentJob); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON analysis_jobs
		FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected legacy CodeIndex Job insert failure'`).Error; err != nil {
		t.Fatalf("create handoff failure trigger: %v", err)
	}
	baseDir := t.TempDir()
	t.Cleanup(func() { makeLegacyHandoffTreeWritable(baseDir) })
	storeFS := snapshotstore.NewLocalSnapshotStore(baseDir)
	handler := indexing.NewSnapshotJobHandler(
		repoStore, snapshotStore, nil, storeFS, &legacyHandoffTestCloner{commitSHA: commitSHA},
		indexing.NewFileFilter(512), indexing.NewCodeChunker(5, 2), nil,
	).WithCodeIntelStore(ciStore)
	worker := startLegacyHandoffWorker(t, jobsStore, jobs.JobTypeMaterializeSnapshot, handler)
	worker.RegisterHandler(jobs.JobTypeBuildCodeIndex, jobs.HandlerFunc(func(context.Context, *jobs.AnalysisJob) error { return nil }))

	failedJob := waitForRealLegacyJobStatus(t, jobsStore, parentJob.ID, jobs.StatusRetryWait)
	if failedJob.Status == jobs.StatusSucceeded || failedJob.LastErrorCode == nil || *failedJob.LastErrorCode != "CODE_INDEX_HANDOFF_FAILED" {
		t.Fatalf("failed Snapshot handoff job=%+v; want RETRY_WAIT with handoff error", failedJob)
	}
	snapAfterFailure, err := snapshotStore.GetByID(ctx, snapshotID)
	if err != nil || snapAfterFailure.Status != snapshot.StatusMaterializing {
		t.Fatalf("Snapshot after failed transaction=%+v err=%v; want MATERIALIZING", snapAfterFailure, err)
	}
	var buildCount, childJobCount int64
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("snapshot_id = ?", snapshotID).Count(&buildCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildCodeIndex, childJobResource).Count(&childJobCount).Error; err != nil {
		t.Fatal(err)
	}
	if buildCount != 1 || childJobCount != 0 {
		t.Fatalf("failed handoff left CodeIndexBuilds=%d child Jobs=%d; want the existing single build and no Job", buildCount, childJobCount)
	}
	if err := db.Exec("DROP TRIGGER " + trigger).Error; err != nil {
		t.Fatalf("drop handoff failure trigger: %v", err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", parentJob.ID).Update("next_run_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if job := waitForRealLegacyJobStatus(t, jobsStore, parentJob.ID, jobs.StatusSucceeded); job.Status != jobs.StatusSucceeded {
		t.Fatalf("Snapshot Job after retry=%s; want SUCCEEDED", job.Status)
	}
	finalSnapshot, err := snapshotStore.GetByID(ctx, snapshotID)
	if err != nil || finalSnapshot.Status != snapshot.StatusReady || finalSnapshot.MaterializedPath == "" {
		t.Fatalf("Snapshot after retry=%+v err=%v; want READY", finalSnapshot, err)
	}
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("snapshot_id = ?", snapshotID).Count(&buildCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildCodeIndex, childJobResource).Count(&childJobCount).Error; err != nil {
		t.Fatal(err)
	}
	if buildCount != 1 || childJobCount != 1 {
		t.Fatalf("Snapshot retry produced CodeIndexBuilds=%d child Jobs=%d; want exactly one each", buildCount, childJobCount)
	}
	if job := waitForRealLegacyJobStatus(t, jobsStore, childBuildJobID(t, jobsStore, jobs.JobTypeBuildCodeIndex, childJobResource), jobs.StatusSucceeded); job.Status != jobs.StatusSucceeded {
		t.Fatalf("BUILD_CODE_INDEX Job status after no-op test handler=%s; want SUCCEEDED", job.Status)
	}
}

func TestRealMySQL_LegacyCodeIndexHandoffIsAtomic(t *testing.T) {
	if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "" {
		t.Skip("skipping real MySQL legacy handoff test (set REPOLENS_REQUIRE_REAL_INTEGRATION=1)")
	}
	db, jobsStore, cleanup := setupRealMySQL(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	const (
		repoID     = "repo-mysql-legacy-codeindex-atomic"
		snapshotID = "snap-mysql-legacy-codeindex-atomic"
		trigger    = "fail_legacy_retrieval_job"
	)
	t.Cleanup(func() { _ = db.Exec("DROP TRIGGER IF EXISTS " + trigger).Error })
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	sourceDir, err := storeFS.EnsureDir(repoID, snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte("module example.com/mysql-legacy-code\n\ngo 1.22\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte("package main\n\nfunc Hello() {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	snapshotStore := snapshot.NewStore(db)
	readyAt := time.Now().UTC()
	if err := snapshotStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapshotID, RepositoryID: repoID, CommitSHA: "0123456789abcdef0123456789abcdef01234567",
		Ref: "main", MaterializedPath: sourceDir, Status: snapshot.StatusReady, ReadyAt: &readyAt,
	}); err != nil {
		t.Fatal(err)
	}
	ciStore := codeintelstore.NewStore(db)
	codeIndexBuild, created, err := ciStore.GetOrCreateBuild(ctx, snapshotID, "example.com/mysql-legacy-code", codeintelmodel.DefaultBuildContext())
	if err != nil || !created || codeIndexBuild.Status != codeintelmodel.BuildStatusCreated {
		t.Fatalf("initial CodeIndexBuild=%+v created=%t err=%v; want CREATED", codeIndexBuild, created, err)
	}
	retrievalBuild, retrievalCreated, err := ciStore.GetOrCreateRetrievalBuild(ctx, codeIndexBuild.ID, "BM25")
	if err != nil || !retrievalCreated {
		t.Fatalf("pre-create RetrievalBuild=%+v created=%t err=%v", retrievalBuild, retrievalCreated, err)
	}
	retrievalJobResource := fmt.Sprintf("%d", retrievalBuild.ID)
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildRetrieval, retrievalJobResource).Delete(&jobs.AnalysisJob{}).Error; err != nil {
		t.Fatalf("remove pre-created Retrieval Job: %v", err)
	}
	if err := db.Exec(`CREATE TRIGGER ` + trigger + ` BEFORE INSERT ON analysis_jobs
		FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'injected legacy Retrieval Job insert failure'`).Error; err != nil {
		t.Fatalf("create handoff failure trigger: %v", err)
	}
	handler := codeintel.NewCodeIndexJobHandler(ciStore, snapshotStore, storeFS, codeintel.NewAnalyzer())
	worker := startLegacyHandoffWorker(t, jobsStore, jobs.JobTypeBuildCodeIndex, handler)
	worker.RegisterHandler(jobs.JobTypeBuildRetrieval, jobs.HandlerFunc(func(context.Context, *jobs.AnalysisJob) error { return nil }))
	codeIndexJob := childBuildJobID(t, jobsStore, jobs.JobTypeBuildCodeIndex, fmt.Sprintf("%d", codeIndexBuild.ID))

	failedJob := waitForRealLegacyJobStatus(t, jobsStore, codeIndexJob, jobs.StatusRetryWait)
	if failedJob.Status == jobs.StatusSucceeded || failedJob.LastErrorCode == nil || *failedJob.LastErrorCode != "RETRIEVAL_HANDOFF_FAILED" {
		t.Fatalf("failed CodeIndex handoff job=%+v; want RETRY_WAIT with handoff error", failedJob)
	}
	buildAfterFailure, err := ciStore.GetByID(ctx, codeIndexBuild.ID)
	if err != nil || buildAfterFailure.Status != codeintelmodel.BuildStatusBuilding {
		t.Fatalf("CodeIndexBuild after failed transaction=%+v err=%v; want BUILDING", buildAfterFailure, err)
	}
	symbols, err := ciStore.ListAllSymbols(ctx, codeIndexBuild.ID)
	if err != nil || len(symbols) != 0 {
		t.Fatalf("analysis symbols after failed transaction=%d err=%v; want zero", len(symbols), err)
	}
	var retrievalCount, retrievalJobCount int64
	if err := db.Model(&codeintelmodel.RetrievalBuild{}).Where("id = ?", retrievalBuild.ID).Count(&retrievalCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildRetrieval, retrievalJobResource).Count(&retrievalJobCount).Error; err != nil {
		t.Fatal(err)
	}
	if retrievalCount != 1 || retrievalJobCount != 0 {
		t.Fatalf("failed handoff left RetrievalBuilds=%d child Jobs=%d; want the existing single build and no Job", retrievalCount, retrievalJobCount)
	}
	if err := db.Exec("DROP TRIGGER " + trigger).Error; err != nil {
		t.Fatalf("drop handoff failure trigger: %v", err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", codeIndexJob).Update("next_run_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	if job := waitForRealLegacyJobStatus(t, jobsStore, codeIndexJob, jobs.StatusSucceeded); job.Status != jobs.StatusSucceeded {
		t.Fatalf("BUILD_CODE_INDEX Job after retry=%s; want SUCCEEDED", job.Status)
	}
	readyBuild, err := ciStore.GetByID(ctx, codeIndexBuild.ID)
	if err != nil || readyBuild.Status != codeintelmodel.BuildStatusReady || readyBuild.SymbolCount == 0 {
		t.Fatalf("CodeIndexBuild after retry=%+v err=%v; want READY with analysis result", readyBuild, err)
	}
	if err := db.Model(&codeintelmodel.RetrievalBuild{}).Where("code_index_build_id = ?", codeIndexBuild.ID).Count(&retrievalCount).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildRetrieval, retrievalJobResource).Count(&retrievalJobCount).Error; err != nil {
		t.Fatal(err)
	}
	if retrievalCount != 1 || retrievalJobCount != 1 {
		t.Fatalf("CodeIndex retry produced RetrievalBuilds=%d child Jobs=%d; want exactly one each", retrievalCount, retrievalJobCount)
	}
	if job := waitForRealLegacyJobStatus(t, jobsStore, childBuildJobID(t, jobsStore, jobs.JobTypeBuildRetrieval, retrievalJobResource), jobs.StatusSucceeded); job.Status != jobs.StatusSucceeded {
		t.Fatalf("BUILD_RETRIEVAL Job status after no-op test handler=%s; want SUCCEEDED", job.Status)
	}
}

type legacyHandoffTestCloner struct {
	commitSHA string
}

func (c *legacyHandoffTestCloner) ValidateGitURL(string) error { return nil }

func (c *legacyHandoffTestCloner) CloneTo(_ context.Context, _, _, targetDir string) (string, error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(targetDir, "main.go"), []byte("package main\n\nfunc Hello() {}\n"), 0644); err != nil {
		return "", err
	}
	return c.commitSHA, nil
}

func startLegacyHandoffWorker(t *testing.T, store *jobs.Store, jobType jobs.JobType, handler jobs.Handler) *jobs.Worker {
	t.Helper()
	cfg := jobs.DefaultWorkerConfig()
	cfg.WorkerID = "mysql-legacy-handoff-worker"
	cfg.Concurrency = 1
	cfg.BatchSize = 1
	cfg.PollInterval = 10 * time.Millisecond
	cfg.LeaseDuration = 10 * time.Second
	cfg.ReapInterval = time.Hour
	cfg.BaseBackoff = time.Minute
	cfg.MaxBackoff = time.Minute
	worker := jobs.NewWorker(store, cfg)
	worker.RegisterHandler(jobType, handler)
	worker.Start(context.Background())
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := worker.StopGracefully(stopCtx); err != nil {
			t.Errorf("stop MySQL legacy handoff worker: %v", err)
		}
	})
	return worker
}

func waitForRealLegacyJobStatus(t *testing.T, store *jobs.Store, jobID int64, want jobs.JobStatus) *jobs.AnalysisJob {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job, err := store.GetJobByID(context.Background(), jobID)
		if err == nil && job.Status == want {
			return job
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := store.GetJobByID(context.Background(), jobID)
	t.Fatalf("MySQL AnalysisJob=%+v err=%v; want %s", job, err, want)
	return nil
}

func childBuildJobID(t *testing.T, store *jobs.Store, jobType jobs.JobType, resourceID string) int64 {
	t.Helper()
	job, err := store.GetJobByResource(context.Background(), jobType, resourceID)
	if err != nil {
		t.Fatalf("load %s Job resource=%s: %v", jobType, resourceID, err)
	}
	return job.ID
}

func makeLegacyHandoffTreeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0755)
		}
		return os.Chmod(path, 0644)
	})
}
