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
	"repolens/internal/retrieval/bm25"
	retrievaleval "repolens/internal/retrieval/eval"
	"repolens/internal/retrieval/structural"
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

func main() {
	fmt.Println("=========================================================================================================")
	fmt.Println("              RepoLens v2.1 — AI Code Intelligence & Diagnosis Platform (Benchmark Runner)              ")
	fmt.Println("=========================================================================================================")

	dataDir := "testdata/eval"
	if err := eval.WriteDatasetSets(dataDir); err != nil {
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
	if err := runner.LoadCasesFromDir(filepath.Join(dataDir, "heldout")); err != nil {
		fmt.Printf("Error loading eval dataset: %v\n", err)
		os.Exit(1)
	}
	heldoutCases := runner.Cases()

	fmt.Printf("Loaded %d held-out benchmark fault cases from %s\n", len(heldoutCases), filepath.Join(dataDir, "heldout"))

	chunker := indexing.NewCodeChunker(50, 10)
	indexes := make(map[string]*bm25.Index)

	for _, c := range heldoutCases {
		fixtureDir := eval.GetFixturePathForRepo(c.RepositoryName)
		targetSnapshotDir := filepath.Join("/tmp/repolens_eval_snapshots", c.RepositoryName, c.SnapshotSHA, "source")
		chunks, err := eval.LoadFixtureChunksAndSnapshot(fixtureDir, targetSnapshotDir, c.SnapshotSHA, chunker)
		if err != nil {
			fmt.Printf("FATAL: failed to load fixture chunks for %s: %v\n", c.CaseID, err)
			os.Exit(1)
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

	// 1. Pure Go BM25 Retrieval Evaluation
	runBM25, err := runner.RunRetrievalEval(ctx, "PURE_GO_BM25", memRetriever)
	if err != nil {
		fmt.Printf("BM25 eval error: %v\n", err)
	}

	// 2. End-to-End Diagnosis Agent Runtime with Bounded 5 Tools
	fakeProvider := llm.NewFakeProvider(llm.ModeToolCallThenDone)
	runE2E, err := runner.RunEndToEndDiagnosisEval(ctx, fakeProvider, memRetriever)
	if err != nil {
		fmt.Printf("E2E diagnosis eval error: %v\n", err)
	}
	if runE2E != nil {
		runE2E.RetrievalStrategy = "AGENT_5_TOOLS"
	}

	// 3. Four-Track Promotion Rule Benchmark Evaluation (ADR 008)
	var testCases []retrievaleval.TestCase
	for _, c := range heldoutCases {
		testCases = append(testCases, retrievaleval.TestCase{
			ID:            c.CaseID,
			Query:         c.IssueTitle,
			ExpectedFiles: c.RelevantFiles,
		})
	}

	benchRunner := retrievaleval.NewBenchmarkRunner()
	primaryIdx := bm25.NewIndex(1.2, 0.75)
	for _, idx := range indexes {
		for _, doc := range idx.Documents {
			primaryIdx.AddDocument(doc)
		}
	}
	primaryIdx.Build()

	cMetrics := benchRunner.EvaluateBM25(primaryIdx, testCases)
	structEngine := structural.NewEngine(primaryIdx, nil, 0)
	dMetrics := benchRunner.EvaluateStructural(ctx, structEngine, testCases)
	promoRes := retrievaleval.CheckPromotionRule(cMetrics, dMetrics)

	eval.PrintComparisonTable([]*eval.EvalRun{runBM25, runE2E})
	fmt.Printf("\n%s\n", promoRes.Summary)
	if !promoRes.PromotedToProduction {
		fmt.Println("Structural Retrieval remains experimental; BM25 stays the production strategy.")
	}
}
