package eval_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/eval"
	"repolens/internal/indexing"
	"repolens/internal/llm"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
)

func TestRetrievalAndEvalBenchmark(t *testing.T) {
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

	// Populate chunk store and disk files for all 32 cases
	for _, c := range eval.StandardFaultCases {
		var chunks []indexing.CodeChunk
		for _, relFile := range c.RelevantFiles {
			mockContent := fmt.Sprintf("// Package code\npackage main\n\nfunc %s() {\n    // %s\n    // %s\n}\n",
				"ResolveFailure",
				c.ExpectedRootCause,
				c.IssueDescription,
			)
			fileChunks := chunker.ChunkFile(c.SnapshotSHA, relFile, mockContent)
			chunks = append(chunks, fileChunks...)

			// Write source file to disk so ReadFileTool and CitationValidator can read it
			filePath := filepath.Join(tmpDir, c.RepositoryName, c.SnapshotSHA, "source", relFile)
			_ = os.MkdirAll(filepath.Dir(filePath), 0755)
			_ = os.WriteFile(filePath, []byte(mockContent), 0644)
		}
		// Add distractor chunks
		for i := 1; i <= 5; i++ {
			dPath := fmt.Sprintf("pkg/mock/distractor_%d.go", i)
			dContent := fmt.Sprintf("// Package distractor\npackage mock\nfunc Helper_%d() {}\n", i)
			chunks = append(chunks, chunker.ChunkFile(c.SnapshotSHA, dPath, dContent)...)

			dFilePath := filepath.Join(tmpDir, c.RepositoryName, c.SnapshotSHA, "source", dPath)
			_ = os.MkdirAll(filepath.Dir(dFilePath), 0755)
			_ = os.WriteFile(dFilePath, []byte(dContent), 0644)
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
	vectorRetriever := retrieval.NewVectorRetriever(chunkStore)
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
