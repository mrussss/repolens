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
)

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

	chunkStore := retrieval.NewMemoryChunkStore()
	chunker := indexing.NewCodeChunker(50, 10)

	// Populate chunk store and disk files for all 32 cases from static fixtures
	for _, c := range eval.StandardFaultCases {
		fixtureDir := eval.GetFixturePathForRepo(c.RepositoryName)
		targetSnapshotDir := filepath.Join(tmpDir, c.RepositoryName, c.SnapshotSHA, "source")
		chunks, err := eval.LoadFixtureChunksAndSnapshot(fixtureDir, targetSnapshotDir, c.SnapshotSHA, chunker)
		if err != nil || len(chunks) == 0 {
			t.Fatalf("failed to load fixture chunks for %s: %v (chunks=%d)", c.CaseID, err, len(chunks))
		}
		chunkStore.SaveChunks(c.SnapshotSHA, chunks)
	}

	ctx := context.Background()

	// 1. BM25 Eval
	bm25Retriever := retrieval.NewBM25Retriever(chunkStore)
	runBM25, err := runner.RunRetrievalEval(ctx, "BM25", bm25Retriever, chunkStore)
	if err != nil {
		t.Fatalf("BM25 eval failed: %v", err)
	}
	if runBM25.Metrics.FileHitAt5 == 0 {
		t.Errorf("expected non-zero Hit@5 for BM25")
	}

	// 2. Vector Eval
	embedder := retrieval.NewLocalHashedFeatureProvider(128)
	vectorRetriever := retrieval.NewVectorRetriever(chunkStore, embedder)
	runVector, err := runner.RunRetrievalEval(ctx, "VECTOR", vectorRetriever, chunkStore)
	if err != nil {
		t.Fatalf("Vector eval failed: %v", err)
	}

	// 3. Hybrid RRF Eval
	hybridRetriever := retrieval.NewHybridRRFRetriever(60, bm25Retriever, vectorRetriever)
	runHybrid, err := runner.RunRetrievalEval(ctx, "HYBRID_RRF", hybridRetriever, chunkStore)
	if err != nil {
		t.Fatalf("Hybrid RRF eval failed: %v", err)
	}
	if runHybrid.Metrics.MRR == 0 {
		t.Errorf("expected non-zero MRR for Hybrid RRF")
	}

	// 4. End-to-End Diagnosis Eval with Real Agent Tool Calling Loop
	fakeProvider := llm.NewFakeProvider(llm.ModeToolCallThenDone)
	runE2E, err := runner.RunEndToEndDiagnosisEval(ctx, fakeProvider, hybridRetriever)
	if err != nil {
		t.Fatalf("E2E diagnosis eval failed: %v", err)
	}

	if runE2E.Metrics.AvgPromptTokens == 0 {
		t.Errorf("expected non-zero token metrics in E2E Eval")
	}

	eval.PrintComparisonTable([]*eval.EvalRun{runBM25, runVector, runHybrid, runE2E})
}
