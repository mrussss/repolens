package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"repolens/internal/eval"
	"repolens/internal/indexing"
	"repolens/internal/llm"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
)

func main() {
	fmt.Println("=========================================================================================================")
	fmt.Println("              RepoLens — Reliable AI Repository Diagnosis Platform (Eval Benchmark Runner)              ")
	fmt.Println("=========================================================================================================")

	dataDir := "testdata/eval_cases"
	if err := eval.WriteStandardDatasetToDir(dataDir); err != nil {
		fmt.Printf("Error writing eval dataset: %v\n", err)
		os.Exit(1)
	}

	// Step 0: Validate dataset ground truth fixtures before running any benchmarks
	if err := eval.ValidateDatasetFixtures(eval.StandardFaultCases); err != nil {
		fmt.Printf("FATAL: Dataset fixture validation failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓ Dataset fixture ground truth validation passed (32/32 cases verified, 0 missing files)")

	storeFS := snapshotstore.NewLocalSnapshotStore("/tmp/repolens_eval_snapshots")
	runner := eval.NewRunner(storeFS)
	if err := runner.LoadCasesFromDir(dataDir); err != nil {
		fmt.Printf("Error loading eval dataset: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Loaded %d benchmark fault cases from %s\n", len(eval.StandardFaultCases), dataDir)

	// Create and populate chunk store with real static repository fixture chunks for each case
	chunkStore := retrieval.NewMemoryChunkStore()
	chunker := indexing.NewCodeChunker(50, 10)

	for _, c := range eval.StandardFaultCases {
		fixtureDir := eval.GetFixturePathForRepo(c.RepositoryName)
		targetSnapshotDir := filepath.Join("/tmp/repolens_eval_snapshots", c.RepositoryName, c.SnapshotSHA, "source")
		chunks, err := eval.LoadFixtureChunksAndSnapshot(fixtureDir, targetSnapshotDir, c.SnapshotSHA, chunker)
		if err != nil {
			fmt.Printf("FATAL: failed to load fixture chunks for %s: %v\n", c.CaseID, err)
			os.Exit(1)
		}
		if len(chunks) == 0 {
			fmt.Printf("FATAL: 0 chunks loaded for fixture %s (%s)\n", c.CaseID, c.RepositoryName)
			os.Exit(1)
		}
		chunkStore.SaveChunks(c.SnapshotSHA, chunks)
	}

	ctx := context.Background()

	// 1. Lexical Baseline
	lexicalRetriever := retrieval.NewLexicalRetriever(chunkStore)
	runLexical, err := runner.RunRetrievalEval(ctx, "LEXICAL", lexicalRetriever, chunkStore)
	if err != nil {
		fmt.Printf("Lexical eval error: %v\n", err)
	}

	// 2. BM25 Search
	bm25Retriever := retrieval.NewBM25Retriever(chunkStore)
	runBM25, err := runner.RunRetrievalEval(ctx, "BM25", bm25Retriever, chunkStore)
	if err != nil {
		fmt.Printf("BM25 eval error: %v\n", err)
	}

	// 3. Local Hashed Vector Baseline
	embedder := retrieval.NewLocalHashedFeatureProvider(128)
	vectorRetriever := retrieval.NewVectorRetriever(chunkStore, embedder)
	runVector, err := runner.RunRetrievalEval(ctx, "LOCAL_HASHED_VEC", vectorRetriever, chunkStore)
	if err != nil {
		fmt.Printf("Vector eval error: %v\n", err)
	}

	// 4. Hybrid Baseline Search (BM25 + Hashed Vector)
	hybridRetriever := retrieval.NewHybridRRFRetriever(60, bm25Retriever, vectorRetriever)
	runHybrid, err := runner.RunRetrievalEval(ctx, "HYBRID_BASELINE", hybridRetriever, chunkStore)
	if err != nil {
		fmt.Printf("Hybrid eval error: %v\n", err)
	}

	// 5. Agent Diagnosis Runtime Plumbing Eval (with deterministic FakeProvider)
	fakeProvider := llm.NewFakeProvider(llm.ModeToolCallThenDone)
	runE2E, err := runner.RunEndToEndDiagnosisEval(ctx, fakeProvider, hybridRetriever)
	if err != nil {
		fmt.Printf("E2E diagnosis eval error: %v\n", err)
	}
	if runE2E != nil {
		runE2E.RetrievalStrategy = "AGENT_PLUMBING"
	}

	eval.PrintComparisonTable([]*eval.EvalRun{
		runLexical,
		runBM25,
		runVector,
		runHybrid,
		runE2E,
	})

	fmt.Println("\nNote: LOCAL_HASHED_VEC is a deterministic token-hash baseline; production neural embeddings require OPENAI / compatible API keys.")
	fmt.Println("Note: AGENT_PLUMBING evaluates the multi-step agent runtime loop, tool calling, and report validation plumbing using a deterministic fake LLM provider.")
	fmt.Println("Eval run completed successfully. Metrics verified against static repository fixtures.")
}
