package snapshotstore_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestReadFileRangeRejectsStartBeyondFileAndParentSymlink(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadFileRange(context.Background(), "repo", "snap", "main.go", 99, 0, 0); err == nil {
		t.Fatal("start line beyond file was accepted")
	}

	nested := filepath.Join(root, "nested")
	if err := os.MkdirAll(nested, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "nested.go"), []byte("package nested\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link-dir")
	if err := os.Symlink(nested, link); err == nil {
		if _, err := store.ReadFileRange(context.Background(), "repo", "snap", "link-dir/nested.go", 1, 1, 0); err == nil {
			t.Fatal("parent symlink was accepted")
		}
	}
}

func TestReadFileRangeKeepsUTF8LinesIntactWhenTruncated(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "utf8.go"), []byte("第一行\n第二行\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rangeResult, err := store.ReadFileRange(context.Background(), "repo", "snap", "utf8.go", 1, 0, len([]byte("第一行")))
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(rangeResult.Content) || rangeResult.Content != "第一行" || rangeResult.EndLine != 1 || !rangeResult.Truncated {
		t.Fatalf("unexpected UTF-8 truncation result: %+v", rangeResult)
	}
}

func TestReadFileRangeStreamsLargeUnselectedLineWithinBoundedRange(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	content := strings.Repeat("x", 8*1024*1024) + "\ntarget\n"
	if err := os.WriteFile(filepath.Join(root, "large.go"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	rangeResult, err := store.ReadFileRange(context.Background(), "repo", "snap", "large.go", 2, 2, 16)
	if err != nil {
		t.Fatal(err)
	}
	if rangeResult.Content != "target" || rangeResult.StartLine != 2 || rangeResult.EndLine != 2 || rangeResult.TotalLines != 3 || rangeResult.Truncated {
		t.Fatalf("large bounded range = %+v", rangeResult)
	}
}

func TestReadFileRangeBoundedStopsAfterRequestedEnd(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	content := "package sample\nfunc Target() {}\n" + strings.Repeat("// irrelevant tail\n", 10000)
	if err := os.WriteFile(filepath.Join(root, "sample.go"), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	rangeResult, err := store.ReadFileRangeBounded(context.Background(), "repo", "snap", "sample.go", 1, 2, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	if rangeResult.Content != "package sample\nfunc Target() {}" || rangeResult.EndLine != 2 {
		t.Fatalf("bounded range = %+v", rangeResult)
	}
	if rangeResult.TotalLines != 0 {
		t.Fatalf("bounded range scanned past the requested end; TotalLines = %d, want unknown (0)", rangeResult.TotalLines)
	}
}

func TestReadFileRangesBoundedExtractsManyRangesInOnePass(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, 10000)
	for i := range lines {
		lines[i] = fmt.Sprintf("line-%d", i+1)
	}
	if err := os.WriteFile(filepath.Join(root, "many.go"), []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	ranges := []snapshotstore.LineRange{{StartLine: 2, EndLine: 3}, {StartLine: 9999, EndLine: 10000}, {StartLine: 3, EndLine: 4}}
	results, err := store.ReadFileRangesBounded(context.Background(), "repo", "snap", "many.go", ranges, 64)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"line-2\nline-3", "line-9999\nline-10000", "line-3\nline-4"}
	if len(results) != len(want) {
		t.Fatalf("got %d range results, want %d", len(results), len(want))
	}
	for i := range want {
		if results[i].Err != nil || results[i].Content != want[i] {
			t.Errorf("range %d = %+v, want content %q", i, results[i], want[i])
		}
	}
}
