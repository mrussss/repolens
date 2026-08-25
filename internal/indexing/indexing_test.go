package indexing_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"repolens/internal/indexing"
	"repolens/internal/mq"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repoindex"
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

type mockIndexStore struct {
	repoindex.Store
	mu         sync.Mutex
	lastStatus repoindex.IndexStatus
	lastErr    string
}

func (m *mockIndexStore) UpdateStatus(ctx context.Context, id string, expectedOldStatus, newStatus repoindex.IndexStatus, readyAt *time.Time, chunkCount, docCount int, errCode string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastStatus = newStatus
	m.lastErr = errCode
	return nil
}

func (m *mockIndexStore) GetStatus() (repoindex.IndexStatus, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastStatus, m.lastErr
}

type mockSnapshotStore struct {
	snapshot.Store
}

func (m *mockSnapshotStore) UpdateStatus(ctx context.Context, id string, oldStatus, newStatus snapshot.SnapshotStatus, readyAt *time.Time) error {
	return nil
}

type failingIndexWriter struct {
	err error
}

func (f *failingIndexWriter) IndexChunks(ctx context.Context, snapshotID string, chunks []indexing.CodeChunk) error {
	return f.err
}

func TestIndexWorker_PartialFailurePropagatesIndexFailed(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "repolens_index_worker_test")
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

	mockBroker := mq.NewMemoryBroker()
	mockSnapStore := &mockSnapshotStore{}
	mockIdxStore := &mockIndexStore{}
	cloner := indexing.NewSafeGitCloner([]string{"github.com"}, 50, 1*time.Minute)
	filter := indexing.NewFileFilter(512)
	chunker := indexing.NewCodeChunker(5, 2)
	partialErr := errors.New("elasticsearch bulk indexing partial failure (1 items failed): op=index id=chunk-1 status=400 type=mapper_parsing_exception reason=bad embedding")
	failingWriter := &failingIndexWriter{err: partialErr}

	worker := indexing.NewIndexWorker(
		mockBroker,
		mockSnapStore,
		mockIdxStore,
		storeFS,
		cloner,
		filter,
		chunker,
		failingWriter,
		1,
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		_ = worker.Start(ctx)
	}()
	time.Sleep(20 * time.Millisecond)

	payloadJSON, _ := json.Marshal(indexing.IndexPayload{
		RepositoryID: repoID,
		SnapshotID:   snapID,
		IndexID:      "index-fail-001",
		GitURL:       "https://github.com/repolens/test-repo",
		Ref:          "main",
	})

	err = mockBroker.Publish(ctx, mq.QueueIndexTask, mq.Message{
		ID:        "msg-index-001",
		EventType: "index.task",
		Payload:   string(payloadJSON),
	})
	if err != nil {
		t.Fatalf("failed to publish index task: %v", err)
	}

	// Give worker time to process
	time.Sleep(100 * time.Millisecond)

	status, errCode := mockIdxStore.GetStatus()
	if status != repoindex.StatusIndexFailed {
		t.Fatalf("expected index status to be INDEX_FAILED (%s), got: %s", repoindex.StatusIndexFailed, status)
	}
	if !strings.Contains(errCode, "INDEX_WRITE_FAILED") || !strings.Contains(errCode, "partial failure") {
		t.Fatalf("expected error message to record INDEX_WRITE_FAILED and partial failure, got: %s", errCode)
	}
}
