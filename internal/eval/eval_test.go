package eval_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/eval"
	"repolens/internal/indexing"
	"repolens/internal/llm"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
	"repolens/internal/retrieval/bm25"
)

type MemoryBM25Retriever struct {
	indexes map[string]*bm25.Index
}

func (m *MemoryBM25Retriever) Search(ctx context.Context, req retrieval.SearchRequest) ([]retrieval.SearchResult, error) {
	idx, ok := m.indexes[req.SnapshotID]
	if !ok {
		return nil, nil
	}
	res := idx.Search(req.Query, req.TopK)
	var out []retrieval.SearchResult
	for _, r := range res {
		out = append(out, retrieval.SearchResult{
			ChunkID:         r.Document.FilePath,
			Path:            r.Document.FilePath,
			StartLine:       r.Document.StartLine,
			EndLine:         r.Document.EndLine,
			Snippet:         r.Document.Content,
			Score:           r.Score,
			RetrievalSource: "symbol_bm25_structural",
		})
	}
	return out, nil
}

func TestDatasetGroundTruthValidation(t *testing.T) {
	if err := eval.ValidateDatasetFixtures(eval.StandardFaultCases); err != nil {
		t.Fatalf("dataset ground truth validation failed: %v", err)
	}

	// Verify that an out-of-bounds line range is strictly rejected
	invalidCase := eval.EvalCase{
		CaseID:           "TEST-OUT-OF-BOUNDS",
		RepositoryName:   "repolens/payment-service",
		IssueTitle:       "Test issue title",
		IssueDescription: "Test issue description",
		RelevantFiles:    []string{"internal/platform/config/config.go"},
		RelevantLineRanges: map[string]eval.LineRange{
			"internal/platform/config/config.go": {Start: 1, End: 999999},
		},
	}
	if err := eval.ValidateDatasetFixtures([]eval.EvalCase{invalidCase}); err == nil {
		t.Fatalf("expected error for out-of-bounds line range 1-999999, got nil")
	}

	// Verify that an inverted line range is rejected
	invertedCase := eval.EvalCase{
		CaseID:           "TEST-INVERTED-RANGE",
		RepositoryName:   "repolens/payment-service",
		IssueTitle:       "Test issue title",
		IssueDescription: "Test issue description",
		RelevantFiles:    []string{"internal/platform/config/config.go"},
		RelevantLineRanges: map[string]eval.LineRange{
			"internal/platform/config/config.go": {Start: 20, End: 10},
		},
	}
	if err := eval.ValidateDatasetFixtures([]eval.EvalCase{invertedCase}); err == nil {
		t.Fatalf("expected error for inverted line range 20-10, got nil")
	}
}

func TestRetrievalAndEvalBenchmark(t *testing.T) {
	if err := eval.ValidateDatasetFixtures(eval.StandardFaultCases); err != nil {
		t.Fatalf("dataset ground truth validation failed: %v", err)
	}

	tmpDir, err := os.MkdirTemp("", "repolens_eval_bench")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	storeFS := snapshotstore.NewLocalSnapshotStore(tmpDir)
	runner := eval.NewRunner(storeFS)

	// Populate runner with all 32 standard fault cases
	for _, c := range eval.StandardFaultCases {
		runner.AddCase(c)
	}

	chunker := indexing.NewCodeChunker(50, 10)
	indexes := make(map[string]*bm25.Index)

	// Populate BM25 indexes and disk files for all 32 cases from static fixtures
	for _, c := range eval.StandardFaultCases {
		fixtureDir := eval.GetFixturePathForRepo(c.RepositoryName)
		targetSnapshotDir := filepath.Join(tmpDir, c.RepositoryName, c.SnapshotSHA, "source")
		chunks, err := eval.LoadFixtureChunksAndSnapshot(fixtureDir, targetSnapshotDir, c.SnapshotSHA, chunker)
		if err != nil || len(chunks) == 0 {
			t.Fatalf("failed to load fixture chunks for %s: %v (chunks=%d)", c.CaseID, err, len(chunks))
		}

		idx := bm25.NewIndex(1.2, 0.75)
		for _, ch := range chunks {
			idx.AddDocument(bm25.Document{
				FilePath:   ch.Path,
				StartLine:  ch.StartLine,
				EndLine:    ch.EndLine,
				Content:    ch.Content,
				SymbolName: ch.Symbol,
			})
		}
		idx.Build()
		indexes[c.SnapshotSHA] = idx
	}

	ctx := context.Background()
	memRetriever := &MemoryBM25Retriever{indexes: indexes}

	// 1. Pure Go BM25 Eval
	runBM25, err := runner.RunRetrievalEval(ctx, "BM25", memRetriever)
	if err != nil {
		t.Fatalf("BM25 eval failed: %v", err)
	}
	if runBM25.Metrics.FileHitAt5 == 0 {
		t.Errorf("expected non-zero Hit@5 for BM25")
	}

	// 2. End-to-End Diagnosis Eval with Real Agent Tool Calling Loop
	fakeProvider := llm.NewFakeProvider(llm.ModeToolCallThenDone)
	runE2E, err := runner.RunEndToEndDiagnosisEval(ctx, fakeProvider, memRetriever)
	if err != nil {
		t.Fatalf("E2E diagnosis eval failed: %v", err)
	}

	if runE2E.Metrics.AvgPromptTokens == 0 {
		t.Errorf("expected non-zero token metrics in E2E Eval")
	}

	eval.PrintComparisonTable([]*eval.EvalRun{runBM25, runE2E})
}
