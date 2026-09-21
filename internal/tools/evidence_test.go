package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/evidence"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
)

func TestReadFileToolReturnsStructuredEvidenceHandle(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snap")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	issuer := &toolEvidenceIssuer{}
	tool := NewReadFileTool(store, "repo", "snap").WithEvidenceIssuer(issuer, "attempt", "run", 3, 1024)
	result, err := tool.Execute(context.Background(), `{"path":"main.go","start_line":2,"end_line":2}`)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["evidence_id"] != "ev_tool" || decoded["path"] != "main.go" || decoded["start_line"] != float64(2) || decoded["end_line"] != float64(2) {
		t.Fatalf("unexpected evidence response: %s", result)
	}
}

func TestSearchCodeToolAttachesEvidenceHandle(t *testing.T) {
	issuer := &toolEvidenceIssuer{}
	tool := NewSearchCodeTool(&toolRetriever{}, "snap").WithEvidenceIssuer(nil, issuer, "attempt", "run", 1024)
	result, err := tool.Execute(context.Background(), `{"query":"handler"}`)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []retrieval.SearchResult
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 1 || decoded[0].EvidenceID != "ev_tool" {
		t.Fatalf("unexpected search evidence response: %s", result)
	}
}

type toolEvidenceIssuer struct{}

func (*toolEvidenceIssuer) Issue(_ context.Context, req evidence.IssueRequest) (*evidence.AttemptEvidenceItem, error) {
	return &evidence.AttemptEvidenceItem{ID: "ev_tool", FilePath: req.FilePath, StartLine: req.StartLine, EndLine: req.EndLine, TotalLines: req.EndLine, DisplayExcerpt: "package main", RawContentHash: "hash"}, nil
}
func (*toolEvidenceIssuer) Resolve(context.Context, string, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, evidence.ErrEvidenceNotFound
}

type toolRetriever struct{}

func (*toolRetriever) Search(context.Context, retrieval.SearchRequest) ([]retrieval.SearchResult, error) {
	return []retrieval.SearchResult{{Path: "handler.go", StartLine: 4, EndLine: 8, Snippet: "handler", Score: 1}}, nil
}
