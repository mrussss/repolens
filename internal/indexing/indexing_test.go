package indexing_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"repolens/internal/indexing"
	"repolens/internal/jobs"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
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
		GitURL: "https://github.com/repolens/test-repo",
	}, nil
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

func (m *materializationRecorder) FinalizeMaterialization(ctx context.Context, id, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error {
	m.finalized = true
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

type failingWalkStore struct{ snapshotstore.SnapshotStore }

func (f failingWalkStore) WalkFiles(string, string, func(string, os.FileInfo) error) error {
	return errors.New("simulated walk failure")
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

	sourceDir, err := storeFS.EnsureDir(repoID, snapID)
	if err != nil {
		t.Fatalf("failed to ensure source dir: %v", err)
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
		ID:         1,
		JobType:    jobs.JobTypeMaterializeSnapshot,
		ResourceID: snapID,
	}

	execErr := handler.Execute(ctx, job)
	if execErr == nil {
		t.Fatalf("expected error from handler when indexWriter fails")
	}
	if !strings.Contains(execErr.Error(), "partial failure") {
		t.Errorf("expected error message to contain partial failure, got: %v", execErr)
	}
}
