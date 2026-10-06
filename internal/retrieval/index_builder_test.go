package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/platform/snapshotstore"
)

func TestBuildSymbolIndexPreservesProductionProjection(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root := store.GetSourcePath("repo", "pinned")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package p\nfunc Handle() { bodyNeedle() }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	symbol := &codeintelmodel.Symbol{
		Name: "Handle", QualifiedName: "pkg.Handle", ReceiverCanonical: "ReceiverMarker",
		Signature: "signatureMarker", Doc: "documentationMarker", FilePath: "main.go", StartLine: 2, EndLine: 2,
		SymbolKeyHash: "pinned-key", Kind: codeintelmodel.SymbolKindFunction,
	}
	idx, err := BuildSymbolIndex(context.Background(), []*codeintelmodel.Symbol{symbol}, &SymbolIndexSource{Store: store, RepositoryID: "repo", SnapshotID: "pinned"})
	if err != nil {
		t.Fatal(err)
	}
	if idx.TotalDocs != 1 || idx.K1 != 1.2 || idx.B != .75 {
		t.Fatalf("unexpected index: %+v", idx)
	}
	doc := idx.Documents[0]
	for _, term := range []string{symbol.Name, symbol.QualifiedName, symbol.ReceiverCanonical, symbol.Signature, symbol.Doc, "bodyNeedle"} {
		if !strings.Contains(doc.Content, term) {
			t.Fatalf("missing production content %q: %+v", term, doc)
		}
	}
	if doc.SymbolKeyHash != symbol.SymbolKeyHash || doc.SymbolName != symbol.Name || doc.Kind != string(symbol.Kind) || doc.FilePath != symbol.FilePath || doc.StartLine != 2 || doc.EndLine != 2 {
		t.Fatalf("lost metadata: %+v", doc)
	}
	if len(idx.Search("bodyNeedle", 8)) != 1 {
		t.Fatal("body-only token is not searchable")
	}
}
