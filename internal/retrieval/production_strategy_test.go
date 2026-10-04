package retrieval_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	model "repolens/internal/codeintel/model"
	"repolens/internal/retrieval"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
	retrievaleval "repolens/internal/retrieval/eval"
	"repolens/internal/retrieval/structural"
)

func TestProductionRetrieverExecutesPinnedStrategy(t *testing.T) {
	db, _, store, _ := setupRetrievalDB(t)
	ctx := context.Background()
	cib, _, err := store.GetOrCreateBuild(ctx, "strategy-snapshot", "example.com/m", model.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Model(cib).Update("status", model.BuildStatusReady).Error; err != nil {
		t.Fatal(err)
	}
	idx := bm25.NewIndex(1.2, 0.75)
	idx.AddDocument(bm25.Document{FilePath: "a.go", Content: "needle needle needle needle", SymbolName: "", SymbolKeyHash: "a", StartLine: 2, EndLine: 3})
	idx.AddDocument(bm25.Document{FilePath: "b.go", Content: "needle", SymbolName: "Needle", SymbolKeyHash: "b", StartLine: 4, EndLine: 5})
	idx.Build()
	pure := idx.Search("needle", 1)
	enhanced := structural.NewEngine(idx, nil, 0).Search(ctx, "needle", 1)
	if len(pure) != 1 || pure[0].Document.FilePath != "a.go" || len(enhanced) != 1 || enhanced[0].Document.FilePath != "b.go" {
		t.Fatalf("fixture must distinguish strategies: pure=%+v enhanced=%+v", pure, enhanced)
	}
	samples := []retrievaleval.TestCase{{ID: "strategy", Query: "needle", ExpectedFiles: []string{"a.go"}}}
	runner := retrievaleval.NewBenchmarkRunner()
	promotion := retrievaleval.CheckPromotionRule(runner.EvaluateBM25(idx, samples), runner.EvaluateStructural(ctx, structural.NewEngine(idx, nil, 0), samples))
	if promotion.PromotedToProduction || model.ProductionRetrievalStrategy != model.StrategyBM25 {
		t.Fatalf("unpromoted default must be BM25: %+v", promotion)
	}
	root := t.TempDir()
	retriever := retrieval.NewProductionRetriever(store, root)
	var previousID int64
	var previousHash string
	for _, tc := range []struct{ strategy, path, source string }{{model.ProductionRetrievalStrategy, "a.go", "symbol_bm25"}, {model.StrategyBM25Structural, "b.go", "symbol_bm25_structural"}} {
		t.Run(tc.strategy, func(t *testing.T) {
			rb, _, err := store.GetOrCreateRetrievalBuild(ctx, cib.ID, tc.strategy)
			if err != nil {
				t.Fatal(err)
			}
			if rb.ID == previousID || rb.ConfigHash == previousHash || rb.RetrievalVersion != model.CurrentRetrievalVersion {
				t.Fatal("strategies share a build identity")
			}
			previousID, previousHash = rb.ID, rb.ConfigHash
			path, hash, err := artifact.NewPublisher(root).Publish(rb.ID, 1, "strategy", tc.strategy, idx)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.MarkRetrievalBuilding(ctx, rb.ID); err != nil {
				t.Fatal(err)
			}
			if err = store.CompleteRetrievalBuild(ctx, rb.ID, path, hash, idx.TotalDocs); err != nil {
				t.Fatal(err)
			}
			updateBuild := func(column string, value any) {
				t.Helper()
				if err := db.Model(rb).Update(column, value).Error; err != nil {
					t.Fatal(err)
				}
			}

			req := retrieval.SearchRequest{SnapshotID: cib.SnapshotID, CodeIndexBuildID: cib.ID, RetrievalBuildID: rb.ID, Query: "needle", TopK: 1}
			hits, err := retriever.Search(ctx, req)
			if err != nil || len(hits) != 1 || hits[0].Path != tc.path || hits[0].RetrievalSource != tc.source {
				t.Fatalf("wrong strategy execution: hits=%+v err=%v", hits, err)
			}
			if tc.strategy == model.StrategyBM25 && (hits[0].Score != pure[0].Score || hits[0].RetrievalReason != "BM25" || hits[0].Snippet != "needle needle needle needle" || hits[0].StartLine != 2 || len(hits[0].SymbolKeys) != 1) {
				t.Fatalf("BM25 mapping changed: %+v", hits)
			}
			// Even after caching, changed metadata must not bypass artifact checks.
			if err = db.Model(rb).Update("artifact_hash", "incorrect").Error; err != nil {
				t.Fatal(err)
			}
			if _, err = retriever.Search(ctx, req); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
				t.Fatalf("missing hash validation: %v", err)
			}
			updateBuild("artifact_hash", hash)
			updateBuild("strategy", "UNKNOWN")
			if _, err = retriever.Search(ctx, req); !errors.Is(err, retrieval.ErrUnsupportedStrategy) {
				t.Fatalf("unsupported strategy error: %v", err)
			}
			otherStrategy := model.StrategyBM25
			if tc.strategy == otherStrategy {
				otherStrategy = model.StrategyBM25Structural
			}
			updateBuild("strategy", otherStrategy)
			if _, err = retriever.Search(ctx, req); err == nil || !strings.Contains(err.Error(), "strategy mismatch") {
				t.Fatalf("missing manifest strategy check: %v", err)
			}
			updateBuild("strategy", tc.strategy)
			updateBuild("status", model.BuildStatusBuilding)
			if _, err = retriever.Search(ctx, req); err == nil {
				t.Fatal("non READY accepted")
			}
			updateBuild("status", model.BuildStatusReady)
			req.SnapshotID = "other-snapshot"
			if _, err = retriever.Search(ctx, req); err == nil {
				t.Fatal("wrong snapshot accepted")
			}
			req.SnapshotID = cib.SnapshotID
			updateBuild("code_index_build_id", cib.ID+100)
			if _, err = retriever.Search(ctx, req); err == nil {
				t.Fatal("wrong lineage accepted")
			}
			updateBuild("code_index_build_id", cib.ID)
		})
	}
}
