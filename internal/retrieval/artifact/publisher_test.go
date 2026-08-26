package artifact_test

import (
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
)

func TestLoadIndexVerifiedRejectsCorruptedArtifact(t *testing.T) {
	idx := bm25.NewIndex(1.2, 0.75)
	idx.AddDocument(bm25.Document{FilePath: "main.go", StartLine: 1, EndLine: 2, Content: "func HandleRequest() {}"})
	idx.Build()
	publisher := artifact.NewPublisher(t.TempDir())
	path, hash, err := publisher.Publish(42, "claim-token", "BM25", idx)
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

func TestLoadIndexVerifiedRejectsWrongBuildIdentity(t *testing.T) {
	idx := bm25.NewIndex(1.2, 0.75)
	idx.AddDocument(bm25.Document{FilePath: "main.go", Content: "func Run() {}"})
	idx.Build()
	path, hash, err := artifact.NewPublisher(t.TempDir()).Publish(7, "claim-token", "BM25", idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifact.LoadIndexVerified(path, 8, hash); err == nil {
		t.Fatal("artifact with a different retrieval build ID was accepted")
	}
}
