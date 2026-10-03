package indexing_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/indexing"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

func TestSSRFAndGitURLValidation(t *testing.T) {
	cloner := indexing.NewSafeGitCloner([]string{"github.com", "gitlab.com"}, 50, 1*time.Minute)

	tests := []struct {
		url     string
		allowed bool
	}{
		{"https://github.com/repolens/sample-repo", true},
		{"https://gitlab.com/repolens/sample-repo", true},
		{"http://github.com/repolens/sample-repo", false},   // Non-HTTPS denied
		{"file:///etc/passwd", false},                       // file:// protocol denied
		{"ssh://git@github.com/repolens/repo", false},       // SSH denied
		{"https://127.0.0.1/repolens/repo", false},          // Loopback IP denied
		{"https://10.0.0.1/repolens/repo", false},           // Private RFC1918 denied
		{"https://169.254.169.254/latest/meta-data", false}, // Link-local metadata denied
		{"https://malicious-host.com/repo", false},          // Unallowed host denied
	}

	for _, tt := range tests {
		err := cloner.ValidateGitURL(tt.url)
		if (err == nil) != tt.allowed {
			t.Errorf("url %s: expected allowed=%v, got err=%v", tt.url, tt.allowed, err)
		}
	}
}

func TestFileFilter(t *testing.T) {
	filter := indexing.NewFileFilter(512)

	tests := []struct {
		relPath string
		size    int64
		ignore  bool
	}{
		{"main.go", 1024, false},
		{"internal/service.go", 2048, false},
		{".git/config", 500, true},
		{"node_modules/express/index.js", 1000, true},
		{"vendor/github.com/pkg/pkg.go", 1000, true},
		{".env", 200, true},
		{".env.production", 300, true},
		{"id_rsa", 1600, true},
		{"secret.key", 1200, true},
		{"binary.exe", 5000, true},
		{"image.png", 10000, true},
		{"huge_file.go", 600 * 1024, true}, // Exceeds 512KB limit
	}

	for _, tt := range tests {
		got := filter.ShouldIgnoreFile(tt.relPath, tt.size)
		if got != tt.ignore {
			t.Errorf("file %s (size %d): expected ignore=%v, got %v", tt.relPath, tt.size, tt.ignore, got)
		}
	}
}

func TestCodeChunker(t *testing.T) {
	chunker := indexing.NewCodeChunker(5, 2)

	goCode := `package main

import "fmt"

func CalculateSum(a, b int) int {
    return a + b
}

func HandleUserRequest() {
    fmt.Println("handling")
}
`

	chunks := chunker.ChunkFile("snap-1", "main.go", goCode)
	if len(chunks) == 0 {
		t.Fatalf("expected chunks generated, got 0")
	}

	// Verify symbol extraction and line ranges
	hasFuncSymbol := false
	for _, ch := range chunks {
		if ch.Path != "main.go" {
			t.Errorf("expected path main.go, got %s", ch.Path)
		}
		if ch.Language != "go" {
			t.Errorf("expected language go, got %s", ch.Language)
		}
		if ch.StartLine <= 0 || ch.EndLine < ch.StartLine {
			t.Errorf("invalid line range: %d to %d", ch.StartLine, ch.EndLine)
		}
		if ch.ContentHash == "" {
			t.Errorf("missing content hash")
		}
		if ch.Symbol == "CalculateSum" || ch.Symbol == "HandleUserRequest" {
			hasFuncSymbol = true
		}
	}

	if !hasFuncSymbol {
		t.Errorf("expected at least one chunk to have extracted function symbol")
	}
}

func TestSnapshotStoreSecurityGuards(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "repolens_snap_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storeFS := snapshotstore.NewLocalSnapshotStore(tmpDir)
	repoID := "repo-sec"
	snapID := "snap-sec"

	sourceDir, err := storeFS.EnsureDir(repoID, snapID)
	if err != nil {
		t.Fatalf("failed to create snapshot dir: %v", err)
	}

	// Write safe file
	safeFile := filepath.Join(sourceDir, "app.go")
	_ = os.WriteFile(safeFile, []byte("package app\nfunc Run() {}\n"), 0644)

	ctx := context.Background()

	// 1. Safe read
	content, err := storeFS.ReadFile(ctx, repoID, snapID, "app.go", 1, 2)
	if err != nil || content == "" {
		t.Fatalf("failed to read safe file: %v", err)
	}

	// 2. Path traversal attempt
	_, err = storeFS.ReadFile(ctx, repoID, snapID, "../../../etc/passwd", 1, 10)
	if err == nil {
		t.Fatalf("expected error on path traversal, got nil")
	}
}

type mockRepoStore struct {
	repo.Store
}

func (m *mockRepoStore) GetByID(ctx context.Context, id string) (*repo.Repository, error) {
	return &repo.Repository{
		ID:     id,
		Name:   "example.com/test-repo",
		GitURL: "https://github.com/repolens/test-repo",
	}, nil
}

type failFirstCodeIndexHandoffStore struct {
	codeintelstore.Store
	calls int
}

func (s *failFirstCodeIndexHandoffStore) GetOrCreateBuild(ctx context.Context, snapshotID, modulePath string, bc codeintelmodel.BuildContext) (*codeintelmodel.CodeIndexBuild, bool, error) {
	s.calls++
	if s.calls == 1 {
		return nil, false, fmt.Errorf("injected transient CodeIndexBuild creation failure")
	}
	return s.Store.GetOrCreateBuild(ctx, snapshotID, modulePath, bc)
}

func TestSnapshotHandoffFailurePropagatesAndRetryCreatesOneCodeIndexBuild(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "snapshot_handoff.db?_busy_timeout=5000&_journal_mode=WAL")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	baseCodeIntelStore := codeintelstore.NewStore(db)
	failingCodeIntelStore := &failFirstCodeIndexHandoffStore{Store: baseCodeIntelStore}
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	recorder := &materializationRecorder{snap: &snapshot.RepositorySnapshot{
		ID: "snap-handoff-retry", RepositoryID: "repo-handoff-retry", Ref: "main",
		CommitSHA: commitSHA, Status: snapshot.StatusMaterializing,
	}}
	basePath := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return os.Chmod(path, 0755)
			}
			return os.Chmod(path, 0644)
		})
	})
	storeFS := snapshotstore.NewLocalSnapshotStore(basePath)
	handler := indexing.NewSnapshotJobHandler(
		&mockRepoStore{}, recorder, nil, storeFS,
		&fixtureCloner{commitSHA: commitSHA}, indexing.NewFileFilter(512), indexing.NewCodeChunker(5, 2), nil,
	).WithCodeIntelStore(failingCodeIntelStore)
	job := &jobs.AnalysisJob{ID: 31, ResourceID: recorder.snap.ID, AttemptCount: 1, MaxAttempts: 3}

	firstErr := handler.Execute(ctx, job)
	if firstErr == nil {
		t.Fatal("CodeIndexBuild handoff failure was silently reported as success")
	}
	if class, _ := jobs.ClassifyError(firstErr); class != jobs.ErrorClassRetryable {
		t.Fatalf("handoff failure class=%s err=%v; want retryable", class, firstErr)
	}
	if recorder.snap.Status != snapshot.StatusReady {
		t.Fatalf("parent Snapshot status=%s after handoff error; want READY", recorder.snap.Status)
	}
	if err := handler.Execute(ctx, job); err != nil {
		t.Fatalf("retry of READY Snapshot did not resume handoff: %v", err)
	}
	if failingCodeIntelStore.calls != 2 {
		t.Fatalf("CodeIndexBuild handoff calls=%d; want injected failure plus one retry", failingCodeIntelStore.calls)
	}
	var builds, buildJobs int64
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("snapshot_id = ?", recorder.snap.ID).Count(&builds).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id IN (SELECT CAST(id AS TEXT) FROM code_index_builds WHERE snapshot_id = ?)", jobs.JobTypeBuildCodeIndex, recorder.snap.ID).Count(&buildJobs).Error; err != nil {
		t.Fatal(err)
	}
	if builds != 1 || buildJobs != 1 {
		t.Fatalf("retry created CodeIndexBuild rows=%d jobs=%d; want exactly one each", builds, buildJobs)
	}
}

func TestLegacySnapshotHandoffFailureKeepsJobRetryableAndCreatesOneCodeIndex(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy_snapshot_atomic_handoff.db?_busy_timeout=5000&_journal_mode=WAL")), &gorm.Config{})
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
	ctx := context.Background()
	const (
		repoID     = "repo-legacy-snapshot-atomic"
		snapshotID = "snap-legacy-snapshot-atomic"
		commitSHA  = "0123456789abcdef0123456789abcdef01234567"
	)
	repoStore := repo.NewStore(db)
	if err := repoStore.Create(ctx, &repo.Repository{
		ID: repoID, UserID: "user-legacy-atomic", Name: "example.com/legacy-atomic",
		GitURL: "https://github.com/example/legacy-atomic", DefaultRef: "main", Status: repo.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	snapshotStore := snapshot.NewStore(db)
	if err := snapshotStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapshotID, RepositoryID: repoID, Ref: "main", CommitSHA: commitSHA, Status: snapshot.StatusMaterializing,
	}); err != nil {
		t.Fatal(err)
	}
	jobsStore := jobs.NewStoreWithDriver(sqlDB, "sqlite3")
	parentJob := &jobs.AnalysisJob{JobType: jobs.JobTypeMaterializeSnapshot, ResourceID: snapshotID, MaxAttempts: 3}
	if err := jobsStore.CreateJob(ctx, parentJob); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TRIGGER fail_legacy_code_index_insert BEFORE INSERT ON code_index_builds
		BEGIN SELECT RAISE(FAIL, 'injected CodeIndexBuild create failure'); END`).Error; err != nil {
		t.Fatalf("create handoff failure trigger: %v", err)
	}
	baseDir := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(baseDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return os.Chmod(path, 0755)
			}
			return os.Chmod(path, 0644)
		})
	})
	storeFS := snapshotstore.NewLocalSnapshotStore(baseDir)
	ciStore := codeintelstore.NewStore(db)
	handler := indexing.NewSnapshotJobHandler(
		repoStore, snapshotStore, nil, storeFS, &fixtureCloner{commitSHA: commitSHA},
		indexing.NewFileFilter(512), indexing.NewCodeChunker(5, 2), nil,
	).WithCodeIntelStore(ciStore)
	workerCfg := jobs.DefaultWorkerConfig()
	workerCfg.WorkerID = "legacy-snapshot-atomic-worker"
	workerCfg.Concurrency = 1
	workerCfg.BatchSize = 1
	workerCfg.PollInterval = 5 * time.Millisecond
	workerCfg.LeaseDuration = 5 * time.Second
	workerCfg.ReapInterval = time.Hour
	workerCfg.BaseBackoff = time.Minute
	workerCfg.MaxBackoff = time.Minute
	worker := jobs.NewWorker(jobsStore, workerCfg)
	worker.RegisterHandler(jobs.JobTypeMaterializeSnapshot, handler)
	worker.RegisterHandler(jobs.JobTypeBuildCodeIndex, jobs.HandlerFunc(func(context.Context, *jobs.AnalysisJob) error { return nil }))
	worker.Start(ctx)
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := worker.StopGracefully(stopCtx); err != nil {
			t.Errorf("stop legacy snapshot worker: %v", err)
		}
	})

	failedJob := waitForLegacySnapshotJobStatus(t, jobsStore, parentJob.ID, jobs.StatusRetryWait)
	if failedJob.Status == jobs.StatusSucceeded || failedJob.LastErrorCode == nil || *failedJob.LastErrorCode != "CODE_INDEX_HANDOFF_FAILED" {
		t.Fatalf("first handoff failure job=%+v; want retryable non-success", failedJob)
	}
	snapAfterFailure, err := snapshotStore.GetByID(ctx, snapshotID)
	if err != nil || snapAfterFailure.Status != snapshot.StatusMaterializing {
		t.Fatalf("snapshot after rolled-back handoff=%+v err=%v; want MATERIALIZING", snapAfterFailure, err)
	}
	var codeIndexBuilds, codeIndexJobs int64
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("snapshot_id = ?", snapshotID).Count(&codeIndexBuilds).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id IN (SELECT CAST(id AS TEXT) FROM code_index_builds WHERE snapshot_id = ?)", jobs.JobTypeBuildCodeIndex, snapshotID).Count(&codeIndexJobs).Error; err != nil {
		t.Fatal(err)
	}
	if codeIndexBuilds != 0 || codeIndexJobs != 0 {
		t.Fatalf("failed transaction left CodeIndexBuilds=%d BUILD_CODE_INDEX jobs=%d; want zero", codeIndexBuilds, codeIndexJobs)
	}
	if err := db.Exec("DROP TRIGGER fail_legacy_code_index_insert").Error; err != nil {
		t.Fatalf("drop handoff failure trigger: %v", err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", parentJob.ID).Update("next_run_at", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	succeededJob := waitForLegacySnapshotJobStatus(t, jobsStore, parentJob.ID, jobs.StatusSucceeded)
	finalSnapshot, err := snapshotStore.GetByID(ctx, snapshotID)
	if err != nil || finalSnapshot.Status != snapshot.StatusReady || finalSnapshot.MaterializedPath == "" {
		t.Fatalf("snapshot after retry=%+v err=%v; want READY with materialized path", finalSnapshot, err)
	}
	if succeededJob.Status != jobs.StatusSucceeded {
		t.Fatalf("Snapshot Job after retry=%s; want SUCCEEDED", succeededJob.Status)
	}
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("snapshot_id = ?", snapshotID).Count(&codeIndexBuilds).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id IN (SELECT CAST(id AS TEXT) FROM code_index_builds WHERE snapshot_id = ?)", jobs.JobTypeBuildCodeIndex, snapshotID).Count(&codeIndexJobs).Error; err != nil {
		t.Fatal(err)
	}
	if codeIndexBuilds != 1 || codeIndexJobs != 1 {
		t.Fatalf("successful retry created CodeIndexBuilds=%d BUILD_CODE_INDEX jobs=%d; want exactly one each", codeIndexBuilds, codeIndexJobs)
	}
	childJob, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeBuildCodeIndex, fmt.Sprintf("%d", buildIDForSnapshot(t, ciStore, snapshotID)))
	if err != nil {
		t.Fatalf("load downstream BUILD_CODE_INDEX Job: %v", err)
	}
	waitForLegacySnapshotJobStatus(t, jobsStore, childJob.ID, jobs.StatusSucceeded)
}

func buildIDForSnapshot(t *testing.T, store codeintelstore.Store, snapshotID string) int64 {
	t.Helper()
	build, err := store.GetBySnapshot(context.Background(), snapshotID)
	if err != nil {
		t.Fatalf("load CodeIndexBuild for snapshot %s: %v", snapshotID, err)
	}
	return build.ID
}

func waitForLegacySnapshotJobStatus(t *testing.T, store *jobs.Store, jobID int64, want jobs.JobStatus) *jobs.AnalysisJob {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		job, err := store.GetJobByID(context.Background(), jobID)
		if err == nil && job.Status == want {
			return job
		}
		time.Sleep(5 * time.Millisecond)
	}
	job, err := store.GetJobByID(context.Background(), jobID)
	t.Fatalf("Snapshot Job status=%+v err=%v; want %s", job, err, want)
	return nil
}

type mockSnapshotStore struct {
	snapshot.Store
	lastStatus snapshot.SnapshotStatus
}

func (m *mockSnapshotStore) GetByID(ctx context.Context, id string) (*snapshot.RepositorySnapshot, error) {
	return &snapshot.RepositorySnapshot{
		ID:           id,
		RepositoryID: "repo-fail-test",
		Ref:          "main",
		CommitSHA:    "local-fixture-commit",
		Status:       snapshot.StatusMaterializing,
	}, nil
}

func (m *mockSnapshotStore) UpdateStatus(ctx context.Context, id string, oldStatus, newStatus snapshot.SnapshotStatus, readyAt *time.Time) error {
	m.lastStatus = newStatus
	return nil
}

type materializationRecorder struct {
	snapshot.Store
	snap        *snapshot.RepositorySnapshot
	finalized   bool
	failed      bool
	commitSHA   string
	contentHash string
	fileCount   int
	totalBytes  int64
}

func (m *materializationRecorder) GetByID(ctx context.Context, id string) (*snapshot.RepositorySnapshot, error) {
	return m.snap, nil
}

func (m *materializationRecorder) UpdateStatus(ctx context.Context, id string, oldStatus, newStatus snapshot.SnapshotStatus, readyAt *time.Time) error {
	if newStatus == snapshot.StatusReady {
		m.snap.Status = snapshot.StatusReady
	}
	return nil
}

func (m *materializationRecorder) FinalizeMaterialization(ctx context.Context, id, materializedPath, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error {
	m.finalized = true
	m.snap.MaterializedPath = materializedPath
	m.commitSHA = commitSHA
	m.contentHash = contentHash
	m.fileCount = fileCount
	m.totalBytes = totalBytes
	m.snap.CommitSHA = commitSHA
	m.snap.ContentHash = contentHash
	m.snap.FileCount = fileCount
	m.snap.TotalBytes = totalBytes
	m.snap.Status = snapshot.StatusReady
	m.snap.ReadyAt = &readyAt
	return nil
}

func (m *materializationRecorder) FailMaterialization(ctx context.Context, id, errorCode string) error {
	m.failed = true
	m.snap.Status = snapshot.StatusFailed
	m.snap.ErrorCode = errorCode
	return nil
}

type fixtureCloner struct {
	commitSHA string
}

func (c *fixtureCloner) ValidateGitURL(string) error { return nil }

func (c *fixtureCloner) CloneTo(ctx context.Context, gitURL, ref, targetDir string) (string, error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(targetDir, "main.go"), []byte("package main\nfunc Hello() {}\n"), 0644); err != nil {
		return "", err
	}
	return c.commitSHA, nil
}

type firstCloneBarrier struct {
	commitSHA string
	started   chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	calls     int
}

func (c *firstCloneBarrier) ValidateGitURL(string) error { return nil }

func (c *firstCloneBarrier) CloneTo(ctx context.Context, gitURL, ref, targetDir string) (string, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	content := "current worker\n"
	if call == 1 {
		close(c.started)
		<-c.release
		content = "stale worker\n"
	}
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(targetDir, "main.go"), []byte(content), 0644); err != nil {
		return "", err
	}
	return c.commitSHA, nil
}

type failingWalkStore struct{ snapshotstore.SnapshotStore }

func (f failingWalkStore) WalkFiles(string, string, func(string, os.FileInfo) error) error {
	return errors.New("simulated walk failure")
}

type snapshotRevisionFailureRecorder struct{ calls int }

func (r *snapshotRevisionFailureRecorder) MarkSnapshotReady(context.Context, string, string) error {
	return nil
}

func (r *snapshotRevisionFailureRecorder) MarkFailed(context.Context, string, revision.Stage, string, string) error {
	r.calls++
	return nil
}

func TestStaleSnapshotHandlerDoesNotFailRevision(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "stale_snapshot.db")), &gorm.Config{})
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
	ctx := context.Background()
	const revisionID = "revision-stale-snapshot"
	const snapshotID = "snap-stale-revision"
	const repositoryID = "repo-stale-revision"
	if err := db.Create(&revision.AnalysisRevision{
		ID: revisionID, RepositoryID: repositoryID, SourceRef: "main",
		CommitSHA: "0123456789abcdef0123456789abcdef01234567", PipelineVersion: "v2.2",
		PipelineFingerprint: "fingerprint-stale-snapshot", SnapshotID: snapshotID,
		Status: revision.StatusPreparing, Stage: revision.StageMaterializing, ExecutionGeneration: 1, Version: 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	snapshotStore := snapshot.NewStore(db)
	if err := snapshotStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapshotID, AnalysisRevisionID: revisionID, RepositoryID: repositoryID, Ref: "main",
		CommitSHA: "0123456789abcdef0123456789abcdef01234567", Status: snapshot.StatusMaterializing,
	}); err != nil {
		t.Fatal(err)
	}
	jobsStore := jobs.NewStoreWithDriver(sqlDB, "sqlite3")
	job := &jobs.AnalysisJob{JobType: jobs.JobTypeMaterializeSnapshot, ResourceID: snapshotID, MaxAttempts: 1}
	if err := jobsStore.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	oldClaim, err := jobsStore.ClaimJobs(ctx, "old-snapshot-worker", 1, time.Minute)
	if err != nil || len(oldClaim) != 1 {
		t.Fatalf("old Snapshot claim = %d jobs, err=%v", len(oldClaim), err)
	}
	oldAttempt, err := jobsStore.MarkExecutionStarted(ctx, oldClaim[0].ID, "old-snapshot-worker", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration)
	if err != nil {
		t.Fatal(err)
	}
	oldClaim[0].AttemptCount, oldClaim[0].ExecutionStarted = oldAttempt, true
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", oldClaim[0].ID).Updates(map[string]interface{}{
		"status": jobs.StatusPending, "execution_generation": 2, "attempt_count": 0,
		"worker_id": nil, "claim_token": nil, "lease_until": nil, "next_run_at": time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&revision.AnalysisRevision{}).Where("id = ?", revisionID).Update("execution_generation", 2).Error; err != nil {
		t.Fatal(err)
	}
	newClaim, err := jobsStore.ClaimJobs(ctx, "new-snapshot-worker", 1, time.Minute)
	if err != nil || len(newClaim) != 1 || newClaim[0].ExecutionGeneration != 2 {
		t.Fatalf("new Snapshot claim = %+v, err=%v", newClaim, err)
	}
	revisionFailures := &snapshotRevisionFailureRecorder{}
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	handler := indexing.NewSnapshotJobHandler(
		&mockRepoStore{}, snapshotStore, nil, failingWalkStore{SnapshotStore: storeFS},
		&fixtureCloner{commitSHA: "0123456789abcdef0123456789abcdef01234567"}, indexing.NewFileFilter(512), indexing.NewCodeChunker(5, 2), nil,
	).WithRevisionStore(revisionFailures)
	err = handler.Execute(ctx, oldClaim[0])
	if err == nil {
		t.Fatal("snapshot walk failure unexpectedly succeeded")
	}
	if revisionFailures.calls != 0 {
		t.Fatalf("stale Snapshot handler wrote revision failure %d times without a claim", revisionFailures.calls)
	}
	currentRevision, err := revision.NewStore(db).GetByID(ctx, revisionID)
	if err != nil || currentRevision.Status != revision.StatusPreparing || currentRevision.ExecutionGeneration != 2 {
		t.Fatalf("new revision after stale Snapshot error = %+v err=%v", currentRevision, err)
	}
	currentJob, err := jobsStore.GetJobByID(ctx, newClaim[0].ID)
	if err != nil || currentJob.Status != jobs.StatusRunning || currentJob.ClaimToken == nil || *currentJob.ClaimToken != *newClaim[0].ClaimToken {
		t.Fatalf("new Snapshot claim changed by stale handler: job=%+v err=%v", currentJob, err)
	}
}

func TestStaleSnapshotExecutionCannotReplaceCurrentMaterialization(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "snapshot_artifact_race.db?_busy_timeout=5000&_journal_mode=WAL")), &gorm.Config{})
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
	ctx := context.Background()
	const snapshotID = "snap-immutable-race"
	const repoID = "repo-immutable-race"
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	store := snapshot.NewStore(db)
	basePath := t.TempDir()
	storeFS := snapshotstore.NewLocalSnapshotStore(basePath)
	t.Cleanup(func() {
		_ = filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return os.Chmod(path, 0755)
			}
			return os.Chmod(path, 0644)
		})
	})
	if err := store.Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapshotID, RepositoryID: repoID, CommitSHA: commitSHA, Ref: "main",
		MaterializedPath: filepath.Join(basePath, repoID, snapshotID, "source"), Status: snapshot.StatusMaterializing,
	}); err != nil {
		t.Fatal(err)
	}
	jobsStore := jobs.NewStoreWithDriver(sqlDB, "sqlite3")
	job := &jobs.AnalysisJob{JobType: jobs.JobTypeMaterializeSnapshot, ResourceID: snapshotID, MaxAttempts: 3}
	if err := jobsStore.CreateJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	oldClaim, err := jobsStore.ClaimJobs(ctx, "snapshot-old-worker", 1, time.Minute)
	if err != nil || len(oldClaim) != 1 {
		t.Fatalf("old snapshot claim = %+v, err=%v", oldClaim, err)
	}
	oldAttempt, err := jobsStore.MarkExecutionStarted(ctx, oldClaim[0].ID, "snapshot-old-worker", *oldClaim[0].ClaimToken, oldClaim[0].ExecutionGeneration)
	if err != nil {
		t.Fatal(err)
	}
	oldClaim[0].AttemptCount, oldClaim[0].ExecutionStarted = oldAttempt, true
	barrier := &firstCloneBarrier{commitSHA: commitSHA, started: make(chan struct{}), release: make(chan struct{})}
	handler := indexing.NewSnapshotJobHandler(
		&mockRepoStore{}, store, nil, storeFS, barrier,
		indexing.NewFileFilter(512), indexing.NewCodeChunker(5, 2), nil,
	)
	type executionResult struct{ err error }
	oldDone := make(chan executionResult, 1)
	go func() { oldDone <- executionResult{err: handler.Execute(ctx, oldClaim[0])} }()
	<-barrier.started // W1 is paused after claim and before its filesystem publish.

	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", oldClaim[0].ID).Updates(map[string]interface{}{
		"status": jobs.StatusPending, "execution_generation": 2, "attempt_count": 0,
		"worker_id": nil, "claim_token": nil, "lease_until": nil, "next_run_at": time.Now().UTC(),
	}).Error; err != nil {
		t.Fatal(err)
	}
	newClaim, err := jobsStore.ClaimJobs(ctx, "snapshot-new-worker", 1, time.Minute)
	if err != nil || len(newClaim) != 1 || newClaim[0].ExecutionGeneration != 2 {
		t.Fatalf("new snapshot claim = %+v, err=%v", newClaim, err)
	}
	newAttempt, err := jobsStore.MarkExecutionStarted(ctx, newClaim[0].ID, "snapshot-new-worker", *newClaim[0].ClaimToken, newClaim[0].ExecutionGeneration)
	if err != nil {
		t.Fatal(err)
	}
	newClaim[0].AttemptCount, newClaim[0].ExecutionStarted = newAttempt, true
	if err := handler.Execute(ctx, newClaim[0]); err != nil {
		t.Fatalf("W2 snapshot execution failed: %v", err)
	}
	current, err := store.GetByID(ctx, snapshotID)
	if err != nil || current.Status != snapshot.StatusReady {
		t.Fatalf("W2 snapshot was not made READY: snapshot=%+v err=%v", current, err)
	}
	currentBytes, err := os.ReadFile(filepath.Join(current.MaterializedPath, "main.go"))
	if err != nil || string(currentBytes) != "current worker\n" {
		t.Fatalf("current DB path does not contain W2 output: content=%q err=%v", currentBytes, err)
	}

	close(barrier.release)
	oldResult := <-oldDone
	if !errors.Is(oldResult.err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale W1 finalization error = %v, want ownership lost", oldResult.err)
	}
	oldPath, err := storeFS.GetExecutionSourcePath(repoID, snapshotID, oldClaim[0].ExecutionGeneration, *oldClaim[0].ClaimToken)
	if err != nil || oldPath == current.MaterializedPath {
		t.Fatalf("snapshot executions shared a materialized path: old=%q current=%q err=%v", oldPath, current.MaterializedPath, err)
	}
	oldBytes, err := os.ReadFile(filepath.Join(oldPath, "main.go"))
	if err != nil || string(oldBytes) != "stale worker\n" {
		t.Fatalf("stale execution output was not isolated: content=%q err=%v", oldBytes, err)
	}
	after, err := store.GetByID(ctx, snapshotID)
	if err != nil || after.MaterializedPath != current.MaterializedPath || after.ContentHash != current.ContentHash || after.Status != snapshot.StatusReady {
		t.Fatalf("W1 changed current snapshot pointer or hash: snapshot=%+v err=%v", after, err)
	}
	currentBytes, err = os.ReadFile(filepath.Join(after.MaterializedPath, "main.go"))
	if err != nil || string(currentBytes) != "current worker\n" {
		t.Fatalf("W1 changed W2's published files: content=%q err=%v", currentBytes, err)
	}
}

func TestSnapshotJobHandler_FinalizesOnlyAfterMaterialization(t *testing.T) {
	tmpDir := t.TempDir()
	t.Cleanup(func() {
		// The handler seals READY snapshots read-only. Restore permissions before
		// testing.T removes its temporary directory.
		_ = filepath.Walk(tmpDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() {
				return os.Chmod(path, 0755)
			}
			return os.Chmod(path, 0644)
		})
	})
	storeFS := snapshotstore.NewLocalSnapshotStore(tmpDir)
	commitSHA := "0123456789abcdef0123456789abcdef01234567"
	recorder := &materializationRecorder{snap: &snapshot.RepositorySnapshot{
		ID: "snap-exact", RepositoryID: "repo-exact", Ref: "main", CommitSHA: "pending", Status: snapshot.StatusMaterializing,
	}}
	handler := indexing.NewSnapshotJobHandler(
		&mockRepoStore{}, recorder, nil, storeFS,
		&fixtureCloner{commitSHA: commitSHA}, indexing.NewFileFilter(512), indexing.NewCodeChunker(5, 2), nil,
	)

	if err := handler.Execute(context.Background(), &jobs.AnalysisJob{ID: 11, ResourceID: recorder.snap.ID, AttemptCount: 1, MaxAttempts: 3}); err != nil {
		t.Fatalf("materialization failed: %v", err)
	}
	if !recorder.finalized || recorder.snap.Status != snapshot.StatusReady {
		t.Fatalf("expected finalizer to publish READY snapshot")
	}
	if recorder.commitSHA != commitSHA || recorder.commitSHA == "pending" {
		t.Fatalf("exact commit was not persisted: got %q", recorder.commitSHA)
	}
	if recorder.contentHash == "" || recorder.fileCount != 1 || recorder.totalBytes == 0 {
		t.Fatalf("materialization identity was incomplete: hash=%q files=%d bytes=%d", recorder.contentHash, recorder.fileCount, recorder.totalBytes)
	}

	firstHash := recorder.contentHash
	recorder.finalized = false
	recorder.snap.Status = snapshot.StatusMaterializing
	if err := handler.Execute(context.Background(), &jobs.AnalysisJob{ID: 12, ResourceID: recorder.snap.ID, AttemptCount: 1, MaxAttempts: 3}); err != nil {
		t.Fatalf("repeat materialization failed: %v", err)
	}
	if recorder.contentHash != firstHash {
		t.Fatalf("manifest hash changed for identical source: %s != %s", recorder.contentHash, firstHash)
	}
}

func TestSnapshotJobHandler_DoesNotMarkReadyOnWalkFailure(t *testing.T) {
	recorder := &materializationRecorder{snap: &snapshot.RepositorySnapshot{
		ID: "snap-walk-fail", RepositoryID: "repo-fail-test", Ref: "main", CommitSHA: "0123456789abcdef0123456789abcdef01234567", Status: snapshot.StatusMaterializing,
	}}
	baseFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	handler := indexing.NewSnapshotJobHandler(
		&mockRepoStore{}, recorder, nil, failingWalkStore{SnapshotStore: baseFS},
		&fixtureCloner{commitSHA: recorder.snap.CommitSHA}, indexing.NewFileFilter(512), indexing.NewCodeChunker(5, 2), nil,
	)
	job := &jobs.AnalysisJob{ID: 13, ResourceID: recorder.snap.ID, AttemptCount: 1, MaxAttempts: 3}
	err := handler.Execute(context.Background(), job)
	if err == nil || recorder.snap.Status == snapshot.StatusReady || recorder.finalized {
		t.Fatalf("walk failure must leave snapshot non-READY: err=%v status=%s", err, recorder.snap.Status)
	}
	class, _ := jobs.ClassifyError(err)
	if class != jobs.ErrorClassRetryable {
		t.Fatalf("walk failure should be retryable, got %s", class)
	}

	job.AttemptCount = job.MaxAttempts
	_ = handler.Execute(context.Background(), job)
	if !recorder.failed || recorder.snap.Status != snapshot.StatusFailed {
		t.Fatalf("retry exhaustion must fail snapshot: failed=%v status=%s", recorder.failed, recorder.snap.Status)
	}
}

func TestSnapshotJobHandler_IgnoresOversizedExcludedFilesButRejectsOversizedSource(t *testing.T) {
	claimToken := "fixture-token"
	newFixture := func(t *testing.T, files map[string][]byte) (*indexing.SnapshotJobHandler, *materializationRecorder) {
		t.Helper()
		root := t.TempDir()
		t.Cleanup(func() {
			_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if info.IsDir() {
					return os.Chmod(path, 0755)
				}
				return os.Chmod(path, 0644)
			})
		})
		storeFS := snapshotstore.NewLocalSnapshotStore(root)
		snap := &snapshot.RepositorySnapshot{
			ID: "snap-filter-order", RepositoryID: "repo-filter-order", Ref: "main",
			CommitSHA: "0123456789abcdef0123456789abcdef01234567", Status: snapshot.StatusMaterializing,
		}
		sourceDir, err := storeFS.GetExecutionSourcePath(snap.RepositoryID, snap.ID, 1, claimToken)
		if err != nil {
			t.Fatalf("get execution source dir: %v", err)
		}
		if err := os.MkdirAll(sourceDir, 0755); err != nil {
			t.Fatalf("create execution source dir: %v", err)
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(sourceDir, name), content, 0o644); err != nil {
				t.Fatalf("write fixture %s: %v", name, err)
			}
		}
		recorder := &materializationRecorder{snap: snap}
		handler := indexing.NewSnapshotJobHandler(
			&mockRepoStore{}, recorder, nil, storeFS,
			&fixtureCloner{commitSHA: snap.CommitSHA}, indexing.NewFileFilter(512), indexing.NewCodeChunker(5, 2), nil,
		)
		return handler, recorder
	}

	t.Run("oversized png and ordinary ignored files do not block snapshot", func(t *testing.T) {
		handler, recorder := newFixture(t, map[string][]byte{
			"main.go":   []byte("package main\nfunc Hello() {}\n"),
			"large.png": bytes.Repeat([]byte{0x89}, 513*1024),
			"small.png": []byte("ignored image"),
			".env":      []byte("SECRET=not-indexed"),
		})
		err := handler.Execute(context.Background(), &jobs.AnalysisJob{
			ID: 31, ResourceID: recorder.snap.ID, AttemptCount: 1, MaxAttempts: 3, ExecutionGeneration: 1, ClaimToken: &claimToken,
		})
		if err != nil {
			t.Fatalf("snapshot should ignore excluded files regardless of size: %v", err)
		}
		if !recorder.finalized || recorder.snap.Status != snapshot.StatusReady {
			t.Fatalf("snapshot did not become READY: finalized=%v status=%s", recorder.finalized, recorder.snap.Status)
		}
		if recorder.fileCount != 1 {
			t.Fatalf("indexed file count = %d, want only main.go (ordinary ignored files must remain excluded)", recorder.fileCount)
		}
	})

	t.Run("oversized source still fails", func(t *testing.T) {
		handler, recorder := newFixture(t, map[string][]byte{
			"main.go": bytes.Repeat([]byte{'x'}, 513*1024),
		})
		err := handler.Execute(context.Background(), &jobs.AnalysisJob{
			ID: 32, ResourceID: recorder.snap.ID, AttemptCount: 1, MaxAttempts: 3, ExecutionGeneration: 1, ClaimToken: &claimToken,
		})
		if err == nil {
			t.Fatal("oversized source should fail materialization")
		}
		class, code := jobs.ClassifyError(err)
		if class != jobs.ErrorClassPermanent || code != "FILE_TOO_LARGE" {
			t.Fatalf("oversized source error = %s/%s, want PERMANENT/FILE_TOO_LARGE (%v)", class, code, err)
		}
		if recorder.finalized || recorder.snap.Status == snapshot.StatusReady {
			t.Fatal("oversized source must not publish a READY snapshot")
		}
	})
}

type failingIndexWriter struct {
	err error
}

func (f *failingIndexWriter) IndexChunks(ctx context.Context, snapshotID string, chunks []indexing.CodeChunk) error {
	return f.err
}

func TestSnapshotJobHandler_PartialFailurePropagatesError(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "repolens_snap_handler_test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storeFS := snapshotstore.NewLocalSnapshotStore(tmpDir)
	repoID := "repo-fail-test"
	snapID := "snap-fail-test"
	claimToken := "fixture-token"

	sourceDir, err := storeFS.GetExecutionSourcePath(repoID, snapID, 1, claimToken)
	if err != nil {
		t.Fatalf("failed to get execution source dir: %v", err)
	}
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatalf("failed to create execution source dir: %v", err)
	}
	_ = os.WriteFile(filepath.Join(sourceDir, "main.go"), []byte("package main\nfunc Hello() {}\n"), 0644)

	mockRepo := &mockRepoStore{}
	mockSnap := &mockSnapshotStore{}
	cloner := indexing.NewSafeGitCloner([]string{"github.com"}, 50, 1*time.Minute)
	filter := indexing.NewFileFilter(512)
	chunker := indexing.NewCodeChunker(5, 2)
	partialErr := errors.New("elasticsearch bulk indexing partial failure: bad embedding")
	failingWriter := &failingIndexWriter{err: partialErr}

	handler := indexing.NewSnapshotJobHandler(
		mockRepo,
		mockSnap,
		nil,
		storeFS,
		cloner,
		filter,
		chunker,
		failingWriter,
	)

	ctx := context.Background()
	job := &jobs.AnalysisJob{
		ID:                  1,
		JobType:             jobs.JobTypeMaterializeSnapshot,
		ResourceID:          snapID,
		ExecutionGeneration: 1,
		ClaimToken:          &claimToken,
	}

	execErr := handler.Execute(ctx, job)
	if execErr == nil {
		t.Fatalf("expected error from handler when indexWriter fails")
	}
	if !strings.Contains(execErr.Error(), "partial failure") {
		t.Errorf("expected error message to contain partial failure, got: %v", execErr)
	}
}
