package indexing

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCloneDiskLimitCountsIgnoredFilesAndCleansStaging(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	for path, content := range map[string]string{
		".git/objects/pack/packed-data": "12345678",
		"node_modules/cache.bin":        "abcdefgh",
		"src/main.go":                   "ok",
	} {
		fullPath := filepath.Join(staging, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	cloner := NewSafeGitClonerWithDiskLimit(nil, 16, 0)
	err := cloner.checkCloneDiskLimit(staging)
	if !errors.Is(err, ErrRepositoryCloneSizeLimit) {
		t.Fatalf("size check error = %v, want REPOSITORY_CLONE_SIZE_LIMIT", err)
	}
	if _, statErr := os.Stat(staging); !os.IsNotExist(statErr) {
		t.Fatalf("over-limit staging directory still exists: stat error=%v", statErr)
	}
}

func TestCloneDiskLimitAllowsExactBoundary(t *testing.T) {
	staging := filepath.Join(t.TempDir(), "staging")
	if err := os.MkdirAll(staging, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "source.bin"), []byte("12345678"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := NewSafeGitClonerWithDiskLimit(nil, 8, 0).checkCloneDiskLimit(staging); err != nil {
		t.Fatalf("exact clone-size boundary rejected: %v", err)
	}
}
