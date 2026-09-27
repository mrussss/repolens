package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

func TestSearchCodeBudgetsCandidatesBeforeIssuingEvidence(t *testing.T) {
	issuer := &countingToolEvidenceIssuer{}
	tool := NewSearchCodeTool(&manyResultsRetriever{}, "snap").WithEvidenceIssuer(nil, issuer, "attempt", "run", 1500)
	result, err := tool.Execute(context.Background(), `{"query":"handler","top_k":3}`)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []retrieval.SearchResult
	if err := json.Unmarshal([]byte(result), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded) == 0 || len(decoded) >= 3 || issuer.calls != len(decoded) {
		t.Fatalf("returned rows=%d evidence issues=%d; want a budget-limited result and one issue per returned row: %s", len(decoded), issuer.calls, result)
	}
	if len(result) > 1500 {
		t.Fatalf("search response exceeded the evidence result budget: %d bytes", len(result))
	}
}

func TestSearchCodeTopKIsBoundedAndDeclaredInSchema(t *testing.T) {
	retriever := &requestRecordingRetriever{}
	tool := NewSearchCodeTool(retriever, "snap")
	for _, topK := range []int{0, -1, 21} {
		if _, err := tool.Execute(context.Background(), `{"query":"q","top_k":`+strconv.Itoa(topK)+`}`); err == nil {
			t.Errorf("top_k %d was accepted", topK)
		}
	}
	if retriever.calls != 0 {
		t.Fatalf("retriever called %d times for invalid top_k", retriever.calls)
	}
	properties := tool.Definition().Function.Parameters["properties"].(map[string]interface{})
	topKSchema := properties["top_k"].(map[string]interface{})
	if topKSchema["minimum"] != 1 || topKSchema["maximum"] != 20 {
		t.Fatalf("top_k schema bounds = %v/%v, want 1/20", topKSchema["minimum"], topKSchema["maximum"])
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

type countingToolEvidenceIssuer struct{ calls int }

func (i *countingToolEvidenceIssuer) Issue(_ context.Context, req evidence.IssueRequest) (*evidence.AttemptEvidenceItem, error) {
	i.calls++
	return &evidence.AttemptEvidenceItem{ID: "ev_counted", FilePath: req.FilePath, StartLine: req.StartLine, EndLine: req.EndLine, DisplayExcerpt: "excerpt"}, nil
}
func (*countingToolEvidenceIssuer) Resolve(context.Context, string, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, evidence.ErrEvidenceNotFound
}

type manyResultsRetriever struct{}

func (*manyResultsRetriever) Search(context.Context, retrieval.SearchRequest) ([]retrieval.SearchResult, error) {
	results := make([]retrieval.SearchResult, 3)
	for i := range results {
		results[i] = retrieval.SearchResult{
			ChunkID: "chunk", Path: "src/" + strings.Repeat("p", 400) + ".go", Language: "go", Symbol: "Handler",
			StartLine: 1, EndLine: 3, Snippet: strings.Repeat("snippet", 1000), Score: 0.9, RetrievalSource: "bm25",
		}
	}
	return results, nil
}

type requestRecordingRetriever struct{ calls int }

func (r *requestRecordingRetriever) Search(context.Context, retrieval.SearchRequest) ([]retrieval.SearchResult, error) {
	r.calls++
	return nil, nil
}
