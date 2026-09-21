package snapshotstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/platform/snapshotstore"
)

func TestReadFileRangeNormalizesAndTruncatesAtCompleteLines(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("one\ntwo\nthree\nfour\n"), 0600); err != nil {
		t.Fatal(err)
	}

	rangeResult, err := store.ReadFileRange(context.Background(), "repo", "snap", "main.go", 0, 99, 7)
	if err != nil {
		t.Fatal(err)
	}
	if rangeResult.StartLine != 1 || rangeResult.EndLine != 2 || rangeResult.TotalLines != 5 || rangeResult.Content != "one\ntwo" || !rangeResult.Truncated {
		t.Fatalf("unexpected effective range: %+v", rangeResult)
	}

	last, err := store.ReadFileRange(context.Background(), "repo", "snap", "main.go", 3, 3, 0)
	if err != nil || last.Content != "three" || last.StartLine != 3 || last.EndLine != 3 {
		t.Fatalf("exact range = %+v err=%v", last, err)
	}
}

func TestReadFileRangeRejectsLongLineTraversalAndSymlink(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "long.go"), []byte("123456789\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadFileRange(context.Background(), "repo", "snap", "long.go", 1, 1, 4); !errors.Is(err, snapshotstore.ErrLineTooLong) {
		t.Fatalf("long line error = %v, want ErrLineTooLong", err)
	}
	if _, err := store.ReadFileRange(context.Background(), "repo", "snap", "../long.go", 1, 1, 0); err == nil {
		t.Fatal("path traversal was accepted")
	}
	if err := os.Symlink(filepath.Join(root, "long.go"), filepath.Join(root, "link.go")); err == nil {
		if _, err := store.ReadFileRange(context.Background(), "repo", "snap", "link.go", 1, 1, 0); err == nil {
			t.Fatal("symlink was accepted")
		}
	}
}
