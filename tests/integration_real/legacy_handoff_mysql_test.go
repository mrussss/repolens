package integration_real

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/gorm"

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

func TestRealMySQL_LegacyNullCodeIndexBuildCompletesHandoff(t *testing.T) {
	if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "" {
		t.Skip("skipping real MySQL legacy NULL compatibility test (set REPOLENS_REQUIRE_REAL_INTEGRATION=1)")
	}
	db, jobsStore, cleanup := setupRealMySQL(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	const repoID = "repo-mysql-legacy-null-codeindex"
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{
		ID: repoID, UserID: "user-mysql-legacy-null", Name: "example.com/legacy-null",
		GitURL: "https://github.com/example/legacy-null", DefaultRef: "main", Status: repo.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}

	type legacyCase struct {
		name   string
		status codeintelmodel.BuildStatus
	}
	cases := []legacyCase{
		{name: "created", status: codeintelmodel.BuildStatusCreated},
		{name: "building", status: codeintelmodel.BuildStatusBuilding},
	}
	snapshotStore := snapshot.NewStore(db)
	ciStore := codeintelstore.NewStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	type queuedBuild struct {
		name       string
		buildID    int64
		resourceID string
		jobID      int64
	}
	queued := make([]queuedBuild, 0, len(cases))
	for index, testCase := range cases {
		snapshotID := "snap-mysql-legacy-null-" + testCase.name
		modulePath := "example.com/legacy-null-" + testCase.name
		sourceDir, err := storeFS.EnsureDir(repoID, snapshotID)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte("module "+modulePath+"\n\ngo 1.22\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte("package main\n\nfunc Hello() {}\n"), 0644); err != nil {
			t.Fatal(err)
		}
		readyAt := time.Now().UTC()
		if err := snapshotStore.Create(ctx, &snapshot.RepositorySnapshot{
			ID: snapshotID, RepositoryID: repoID, CommitSHA: strings.Repeat(fmt.Sprintf("%x", index+1), 40),
			Ref: "main", MaterializedPath: sourceDir, ContentHash: strings.Repeat("a", 64),
			Status: snapshot.StatusReady, ReadyAt: &readyAt,
		}); err != nil {
			t.Fatal(err)
		}

		// Insert through SQL to reproduce rows created before migration 002 added
		// the nullable analysis_revision_id column.
		buildID := insertMySQLCodeIndexBuild(t, db, snapshotID, modulePath, testCase.status, nil)
		var revisionID sql.NullString
		var storedStatus codeintelmodel.BuildStatus
		if err := db.Raw("SELECT analysis_revision_id, status FROM code_index_builds WHERE id = ?", buildID).Row().Scan(&revisionID, &storedStatus); err != nil {
			t.Fatalf("read historical CodeIndexBuild row: %v", err)
		}
		if revisionID.Valid || storedStatus != testCase.status {
			t.Fatalf("initial CodeIndexBuild revision=%+v status=%s; want SQL NULL and %s", revisionID, storedStatus, testCase.status)
		}
		loadedBuild, err := ciStore.GetByID(ctx, buildID)
		if err != nil || loadedBuild.AnalysisRevisionID != "" {
			t.Fatalf("Gorm legacy CodeIndexBuild=%+v err=%v; want empty Go revision ID for SQL NULL", loadedBuild, err)
		}
		resourceID := strconv.FormatInt(buildID, 10)
		parentJob := &jobs.AnalysisJob{JobType: jobs.JobTypeBuildCodeIndex, ResourceID: resourceID, MaxAttempts: 3}
		if err := jobsStore.CreateJob(ctx, parentJob); err != nil {
			t.Fatal(err)
		}
		queued = append(queued, queuedBuild{name: testCase.name, buildID: buildID, resourceID: resourceID, jobID: parentJob.ID})
	}

	handler := codeintel.NewCodeIndexJobHandler(ciStore, snapshotStore, storeFS, codeintel.NewAnalyzer())
	worker := startLegacyHandoffWorker(t, jobsStore, jobs.JobTypeBuildCodeIndex, handler)
	worker.RegisterHandler(jobs.JobTypeBuildRetrieval, jobs.HandlerFunc(func(context.Context, *jobs.AnalysisJob) error { return nil }))
	for _, item := range queued {
		parentJob := waitForRealLegacyJobStatus(t, jobsStore, item.jobID, jobs.StatusSucceeded)
		if parentJob.TerminalReason != nil && *parentJob.TerminalReason == jobs.TerminalReasonRetryableExhausted {
			t.Fatalf("%s parent Job unexpectedly exhausted retries: %+v", item.name, parentJob)
		}
		if parentJob.LastErrorCode != nil && *parentJob.LastErrorCode == "RETRIEVAL_HANDOFF_FAILED" {
			t.Fatalf("%s parent Job retained RETRIEVAL_HANDOFF_FAILED: %+v", item.name, parentJob)
		}
		build, err := ciStore.GetByID(ctx, item.buildID)
		if err != nil || build.Status != codeintelmodel.BuildStatusReady || build.SymbolCount == 0 || build.AnalysisRevisionID != "" {
			t.Fatalf("%s legacy CodeIndexBuild after Worker=%+v err=%v; want READY with symbols and legacy Go identity", item.name, build, err)
		}
		var stillNull bool
		if err := db.Raw("SELECT analysis_revision_id IS NULL FROM code_index_builds WHERE id = ?", item.buildID).Scan(&stillNull).Error; err != nil || !stillNull {
			t.Fatalf("%s CodeIndexBuild analysis_revision_id lost SQL NULL: null=%t err=%v", item.name, stillNull, err)
		}
		var retrievalBuildCount, retrievalJobCount int64
		if err := db.Model(&codeintelmodel.RetrievalBuild{}).Where("code_index_build_id = ?", item.buildID).Count(&retrievalBuildCount).Error; err != nil {
			t.Fatal(err)
		}
		retrieval, err := ciStore.GetRetrievalBuildByCodeIndexBuild(ctx, item.buildID)
		if err != nil {
			t.Fatal(err)
		}
		retrievalResourceID := strconv.FormatInt(retrieval.ID, 10)
		if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeBuildRetrieval, retrievalResourceID).Count(&retrievalJobCount).Error; err != nil {
			t.Fatal(err)
		}
		if retrievalBuildCount != 1 || retrievalJobCount != 1 {
			t.Fatalf("%s handoff created RetrievalBuilds=%d BUILD_RETRIEVAL jobs=%d; want exactly one each", item.name, retrievalBuildCount, retrievalJobCount)
		}
		waitForRealLegacyJobStatus(t, jobsStore, childBuildJobID(t, jobsStore, jobs.JobTypeBuildRetrieval, retrievalResourceID), jobs.StatusSucceeded)
	}
}

func TestRealMySQL_LegacyCodeIndexFinalizerRejectsRevisionIdentity(t *testing.T) {
	if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "" {
		t.Skip("skipping real MySQL legacy Revision isolation test (set REPOLENS_REQUIRE_REAL_INTEGRATION=1)")
	}
	db, jobsStore, cleanup := setupRealMySQL(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	const (
		snapshotID = "snap-mysql-revision-isolation"
		modulePath = "example.com/revision-isolation"
		workerID   = "legacy-finalizer-isolation-worker"
		claimToken = "legacy-finalizer-isolation-claim"
	)
	revisionID := "c7d03c1f-2abc-47d1-bc7e-f6db40d874ed"
	buildID := insertMySQLCodeIndexBuild(t, db, snapshotID, modulePath, codeintelmodel.BuildStatusBuilding, &revisionID)
	resourceID := strconv.FormatInt(buildID, 10)
	parentJob := &jobs.AnalysisJob{JobType: jobs.JobTypeBuildCodeIndex, ResourceID: resourceID, MaxAttempts: 3}
	if err := jobsStore.CreateJob(ctx, parentJob); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", parentJob.ID).Updates(map[string]interface{}{
		"status": jobs.StatusRunning, "worker_id": workerID, "claim_token": claimToken,
		"execution_started": true, "lease_until": time.Now().UTC().Add(time.Minute),
	}).Error; err != nil {
		t.Fatal(err)
	}
	ciStore := codeintelstore.NewStore(db)
	err := ciStore.FinalizeLegacyCodeIndexSuccessWithRetrievalHandoff(ctx, parentJob.ID, workerID, claimToken, buildID, &codeintelmodel.AnalysisResult{})
	if err == nil {
		t.Fatal("legacy finalizer accepted a CodeIndexBuild with a non-empty Revision identity")
	}
	build, err := ciStore.GetByID(ctx, buildID)
	if err != nil || build.AnalysisRevisionID != revisionID || build.Status != codeintelmodel.BuildStatusBuilding {
		t.Fatalf("Revision CodeIndexBuild after legacy finalizer=%+v err=%v; want unchanged BUILDING Revision row", build, err)
	}
	job, err := jobsStore.GetJobByID(ctx, parentJob.ID)
	if err != nil || job.Status != jobs.StatusRunning {
		t.Fatalf("Revision BUILD_CODE_INDEX Job after legacy finalizer=%+v err=%v; want RUNNING", job, err)
	}
	var retrievalBuildCount int64
	if err := db.Model(&codeintelmodel.RetrievalBuild{}).Where("code_index_build_id = ?", buildID).Count(&retrievalBuildCount).Error; err != nil {
		t.Fatal(err)
	}
	if retrievalBuildCount != 0 {
		t.Fatalf("Revision row incorrectly created %d legacy RetrievalBuilds", retrievalBuildCount)
	}
}

func insertMySQLCodeIndexBuild(t *testing.T, db *gorm.DB, snapshotID, modulePath string, status codeintelmodel.BuildStatus, revisionID *string) int64 {
	t.Helper()
	buildContext := codeintelmodel.DefaultBuildContext()
	var revisionValue any
	if revisionID != nil {
		revisionValue = *revisionID
	}
	result := db.Exec(`INSERT INTO code_index_builds (
		snapshot_id, analysis_revision_id, parser_version, analyzer_version,
		symbol_schema_version, build_context_hash, module_path, goos, goarch,
		build_tags_hash, build_tags_json, status
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snapshotID, revisionValue, codeintelmodel.CurrentParserVersion, codeintelmodel.CurrentAnalyzerVersion,
		codeintelmodel.CurrentSymbolSchemaVersion, buildContext.BuildContextHash(), modulePath,
		buildContext.GOOS, buildContext.GOARCH, buildContext.BuildTagsHash(), "[]", status,
	)
	if result.Error != nil {
		t.Fatalf("insert CodeIndexBuild row directly: %v", result.Error)
	}
	var build codeintelmodel.CodeIndexBuild
	if err := db.Where("snapshot_id = ? AND parser_version = ? AND analyzer_version = ? AND symbol_schema_version = ? AND build_context_hash = ?",
		snapshotID, codeintelmodel.CurrentParserVersion, codeintelmodel.CurrentAnalyzerVersion,
		codeintelmodel.CurrentSymbolSchemaVersion, buildContext.BuildContextHash()).First(&build).Error; err != nil {
		t.Fatalf("reload manually inserted CodeIndexBuild: %v", err)
	}
	return build.ID
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
