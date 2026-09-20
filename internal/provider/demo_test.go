package provider

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMakeDemoSourceRemovesStaleFixtureFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo", "snapshot", "source")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "main.go")
	if err := os.WriteFile(stale, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := makeDemoSource(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale demo fixture still exists, stat err=%v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("demo source directory was not recreated: info=%v err=%v", info, err)
	}
}
