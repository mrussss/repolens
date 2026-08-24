package indexing_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"repolens/internal/indexing"
	"repolens/internal/platform/snapshotstore"
)

func TestSSRFAndGitURLValidation(t *testing.T) {
	cloner := indexing.NewSafeGitCloner([]string{"github.com", "gitlab.com"}, 50, 1*time.Minute)

	tests := []struct {
		url     string
		allowed bool
	}{
		{"https://github.com/repolens/sample-repo", true},
		{"https://gitlab.com/repolens/sample-repo", true},
		{"http://github.com/repolens/sample-repo", false},         // Non-HTTPS denied
		{"file:///etc/passwd", false},                             // file:// protocol denied
		{"ssh://git@github.com/repolens/repo", false},             // SSH denied
		{"https://127.0.0.1/repolens/repo", false},                // Loopback IP denied
		{"https://10.0.0.1/repolens/repo", false},                 // Private RFC1918 denied
		{"https://169.254.169.254/latest/meta-data", false},       // Link-local metadata denied
		{"https://malicious-host.com/repo", false},                // Unallowed host denied
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
