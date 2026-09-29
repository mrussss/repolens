package agent

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
)

type assemblyRetriever struct{ requests []retrieval.SearchRequest }

func (r *assemblyRetriever) Search(_ context.Context, request retrieval.SearchRequest) ([]retrieval.SearchResult, error) {
	r.requests = append(r.requests, request)
	return []retrieval.SearchResult{{Path: "main.go", StartLine: 1, EndLine: 1, Snippet: "package main", Score: 1}}, nil
}

type assemblyIssuer struct{ issued []evidence.IssueRequest }

func (i *assemblyIssuer) Issue(_ context.Context, request evidence.IssueRequest) (*evidence.AttemptEvidenceItem, error) {
	i.issued = append(i.issued, request)
	return &evidence.AttemptEvidenceItem{ID: "ev_assembly", FilePath: request.FilePath, StartLine: request.StartLine, EndLine: request.EndLine, DisplayExcerpt: "package main"}, nil
}

func (*assemblyIssuer) Resolve(context.Context, string, string) (*evidence.AttemptEvidenceItem, error) {
	return nil, evidence.ErrEvidenceNotFound
}

func TestBuildToolRegistryKeepsFiveReadOnlyToolsAndAttemptEvidence(t *testing.T) {
	store := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := store.EnsureDir("repo", "snapshot")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	retriever := &assemblyRetriever{}
	issuer := &assemblyIssuer{}
	spec := diagnosis.DiagnosisExecutionSpec{RunID: "run"}
	spec.Lineage.RepositoryID = "repo"
	spec.Lineage.SnapshotID = "snapshot"
	spec.Lineage.CodeIndexBuildID = 17
	spec.Lineage.RetrievalBuildID = 23
	spec.Budget.MaxToolResultBytes = 8192
	registry, err := BuildToolRegistry(ToolDependencies{Retriever: retriever, SnapshotStore: store, EvidenceIssuer: issuer}, spec, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	defs := registry.Definitions()
	names := make([]string, 0, len(defs))
	for _, def := range defs {
		if def.Type != "function" {
			t.Fatalf("tool type = %q", def.Type)
		}
		names = append(names, def.Function.Name)
	}
	sort.Strings(names)
	want := []string{"find_references", "find_related_tests", "get_symbol", "read_file", "search_code"}
	if len(names) != len(want) {
		t.Fatalf("tool names = %v, want %v", names, want)
	}
	for n := range names {
		if names[n] != want[n] {
			t.Fatalf("tool names = %v, want %v", names, want)
		}
	}
	search, _ := registry.Get("search_code")
	if _, err := search.Execute(context.Background(), `{"query":"main"}`); err != nil {
		t.Fatal(err)
	}
	if len(retriever.requests) != 1 || retriever.requests[0].CodeIndexBuildID != 17 || retriever.requests[0].RetrievalBuildID != 23 {
		t.Fatalf("pinned search request = %+v", retriever.requests)
	}
	read, _ := registry.Get("read_file")
	if _, err := read.Execute(context.Background(), `{"path":"../main.go"}`); err == nil {
		t.Fatal("read_file allowed path traversal")
	}
	if _, err := read.Execute(context.Background(), `{"path":"main.go","start_line":1,"end_line":1}`); err != nil {
		t.Fatal(err)
	}
	if len(issuer.issued) != 2 {
		t.Fatalf("evidence calls = %+v", issuer.issued)
	}
	for _, request := range issuer.issued {
		if request.AttemptID != "attempt" || request.DiagnosisRunID != "run" || request.RepositoryID != "repo" || request.SnapshotID != "snapshot" || request.CodeIndexBuildID != 17 {
			t.Fatalf("evidence lineage = %+v", request)
		}
	}
	if issuer.issued[1].MaxBytes != 4096 {
		t.Fatalf("read_file evidence budget = %d, want 4096", issuer.issued[1].MaxBytes)
	}
}

func TestBuildToolRegistryKeepsLegacyZeroBuildSearch(t *testing.T) {
	retriever := &assemblyRetriever{}
	spec := diagnosis.DiagnosisExecutionSpec{}
	spec.Lineage.SnapshotID = "snapshot"
	registry, err := BuildToolRegistry(ToolDependencies{Retriever: retriever}, spec, "attempt")
	if err != nil {
		t.Fatal(err)
	}
	search, _ := registry.Get("search_code")
	if _, err := search.Execute(context.Background(), `{"query":"main"}`); err != nil {
		t.Fatal(err)
	}
	if len(retriever.requests) != 1 || retriever.requests[0].CodeIndexBuildID != 0 || retriever.requests[0].RetrievalBuildID != 0 {
		t.Fatalf("legacy search request = %+v", retriever.requests)
	}
	spec.Lineage.CodeIndexBuildID = 17
	if _, err := BuildToolRegistry(ToolDependencies{CodeIntelStore: codeintelstore.NewStore(nil)}, spec, "attempt"); err == nil {
		t.Fatal("incomplete pinned build lineage was accepted")
	}
}
