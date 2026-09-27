package artifact_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
)

func TestLoadIndexVerifiedRejectsCorruptedArtifact(t *testing.T) {
	idx := bm25.NewIndex(1.2, 0.75)
	idx.AddDocument(bm25.Document{FilePath: "main.go", StartLine: 1, EndLine: 2, Content: "func HandleRequest() {}"})
	idx.Build()
	publisher := artifact.NewPublisher(t.TempDir())
	path, hash, err := publisher.Publish(42, 1, "claim-token", "BM25", idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifact.LoadIndexVerified(path, 42, hash); err != nil {
		t.Fatalf("valid artifact was rejected: %v", err)
	}

	if err := os.WriteFile(filepath.Join(path, "index.json"), []byte("corrupted"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := artifact.LoadIndexVerified(path, 42, hash); err == nil {
		t.Fatal("corrupted artifact was accepted")
	}
}

func TestPublisherCleanupRemovesOnlyOldUnreferencedExecutionArtifacts(t *testing.T) {
	root := t.TempDir()
	publisher := artifact.NewPublisher(root)
	idx := bm25.NewIndex(1.2, 0.75)
	idx.AddDocument(bm25.Document{FilePath: "main.go", Content: "marker"})
	idx.Build()
	oldPath, _, err := publisher.Publish(42, 1, "old-claim", "BM25", idx)
	if err != nil {
		t.Fatal(err)
	}
	currentPath, currentHash, err := publisher.Publish(42, 2, "current-claim", "BM25", idx)
	if err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-48 * time.Hour)
	for _, path := range []string{oldPath, currentPath} {
		if err := os.Chtimes(path, oldTime, oldTime); err != nil {
			t.Fatal(err)
		}
	}
	stagingPath := filepath.Join(root, ".tmp", "interrupted-publish")
	if err := os.MkdirAll(stagingPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(stagingPath, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	removed, err := publisher.CleanupUnreferenced(map[string]struct{}{currentPath: {}}, time.Now().Add(-24*time.Hour))
	if err != nil || removed != 2 {
		t.Fatalf("cleanup removed %d paths, err=%v; want old orphan plus stale staging", removed, err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old unreferenced artifact still exists: err=%v", err)
	}
	if _, err := artifact.LoadIndexVerified(currentPath, 42, currentHash); err != nil {
		t.Fatalf("cleanup damaged DB-referenced current artifact: %v", err)
	}
}

func TestLoadIndexVerifiedRejectsWrongBuildIdentity(t *testing.T) {
	idx := bm25.NewIndex(1.2, 0.75)
	idx.AddDocument(bm25.Document{FilePath: "main.go", Content: "func Run() {}"})
	idx.Build()
	path, hash, err := artifact.NewPublisher(t.TempDir()).Publish(7, 1, "claim-token", "BM25", idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifact.LoadIndexVerified(path, 8, hash); err == nil {
		t.Fatal("artifact with a different retrieval build ID was accepted")
	}
}
