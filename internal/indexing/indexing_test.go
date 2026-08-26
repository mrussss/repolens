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
		Status:       snapshot.StatusMaterializing,
	}, nil
}

func (m *mockSnapshotStore) UpdateStatus(ctx context.Context, id string, oldStatus, newStatus snapshot.SnapshotStatus, readyAt *time.Time) error {
	m.lastStatus = newStatus
	return nil
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
