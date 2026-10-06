package retrieval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"repolens/internal/codeintel/model"
	"repolens/internal/retrieval"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
)

func TestProductionV2ExpansionIsPinnedAndHistoricalV1Unchanged(t *testing.T) {
	db, _, store, _ := setupRetrievalDB(t)
	ctx := context.Background()
	cib, _, err := store.GetOrCreateBuild(ctx, "expansion-integration", "example.com/m", model.DefaultBuildContext())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(cib).Update("status", model.BuildStatusReady).Error; err != nil {
		t.Fatal(err)
	}
	idx := bm25.NewIndex(1.2, .75)
	var symbols []model.Symbol
	for i := 0; i < 17; i++ {
		name := fmt.Sprintf("entry%02d", i)
		raw, hash := model.BuildSymbolKey("example.com/m", "example.com/m", "", model.SymbolKindFunction, name)
		sym := model.Symbol{CodeIndexBuildID: cib.ID, SymbolKeyRaw: raw, SymbolKeyHash: hash, Name: name, FilePath: name + ".go", Kind: model.SymbolKindFunction, StartLine: 1, EndLine: 1}
		if err := db.Create(&sym).Error; err != nil {
			t.Fatal(err)
		}
		symbols = append(symbols, sym)
		content := "needle"
		if i == 16 {
			content = "unrelated implementation"
		}
		idx.AddDocument(bm25.Document{FilePath: sym.FilePath, SymbolName: sym.Name, SymbolKeyHash: sym.SymbolKeyHash, Content: content, Kind: string(sym.Kind), StartLine: 1, EndLine: 1})
	}
	idx.Build()
	seed, target := symbols[0], symbols[16]
	relation := model.SymbolRelation{CodeIndexBuildID: cib.ID, FromSymbolID: &seed.ID, FromSymbolKeyHash: seed.SymbolKeyHash, ToSymbolID: &target.ID, ToSymbolKeyHash: target.SymbolKeyHash, RelationType: model.RelationTypeCallCandidate, ResolutionKind: model.ResolutionKindSemantic, Confidence: 1, ReasonCode: "SEMANTIC_DIRECT_FUNC"}
	if err := db.Create(&relation).Error; err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	retriever := retrieval.NewProductionRetriever(store, root)
	ids := map[string]int64{}
	hashes := map[string]bool{}
	for _, strategy := range []string{model.StrategyBM25, model.StrategyBM25Structural, model.StrategyBM25StructuralV2} {
		rb, _, err := store.GetOrCreateRetrievalBuild(ctx, cib.ID, strategy)
		if err != nil {
			t.Fatal(err)
		}
		if hashes[rb.ConfigHash] {
			t.Fatal("shared config identity")
		}
		hashes[rb.ConfigHash] = true
		ids[strategy] = rb.ID
		path, hash, err := artifact.NewPublisher(root).Publish(rb.ID, 1, "expansion", strategy, idx)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.MarkRetrievalBuilding(ctx, rb.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.CompleteRetrievalBuild(ctx, rb.ID, path, hash, idx.TotalDocs); err != nil {
			t.Fatal(err)
		}
	}
	base := idx.Search("needle", 8)
	for _, strategy := range []string{model.StrategyBM25, model.StrategyBM25Structural} {
		hits, trace, err := retriever.SearchWithTrace(ctx, retrieval.SearchRequest{SnapshotID: cib.SnapshotID, CodeIndexBuildID: cib.ID, RetrievalBuildID: ids[strategy], Query: "needle", TopK: 8})
		if err != nil || trace != nil || len(hits) != 8 {
			t.Fatalf("legacy changed: %s %v", strategy, err)
		}
		for i, hit := range hits {
			if hit.SymbolKeys[0] != base[i].Document.SymbolKeyHash || hit.Score != base[i].Score || hit.StructuralV2 != nil {
				t.Fatal("historical result changed")
			}
		}
	}
	req := retrieval.SearchRequest{SnapshotID: cib.SnapshotID, CodeIndexBuildID: cib.ID, RetrievalBuildID: ids[model.StrategyBM25StructuralV2], Query: "needle", TopK: 8}
	var previous string
	for _, env := range []string{"BM25", "BM25_STRUCTURAL_V2", "UNKNOWN"} {
		t.Setenv("REPOLENS_RETRIEVAL_STRATEGY", env)
		hits, trace, err := retriever.SearchWithTrace(ctx, req)
		if err != nil || trace.ExpandedCandidates != 1 || trace.RelationQueries != 16 || len(hits) != 8 {
			t.Fatalf("wrong V2 execution: %+v %v", trace, err)
		}
		hit := hits[2]
		if hit.SymbolKeys[0] != target.SymbolKeyHash || hit.RetrievalSource != "symbol_bm25_structural_v2" || hit.RetrievalReason != "SEMANTIC_CALL_EXPANSION" || hit.StructuralV2 == nil || hit.StructuralV2.FinalRank != 3 || hit.StructuralV2.BestSeed.SeedBM25Rank != 1 || hit.Snippet != "unrelated implementation" {
			t.Fatalf("incomplete mapping: %+v", hit)
		}
		encoded, _ := json.Marshal(hits)
		if previous != "" && previous != string(encoded) {
			t.Fatal("runtime environment changed pinned semantics")
		}
		previous = string(encoded)
		ordinary, err := retriever.Search(ctx, req)
		ordinaryJSON, _ := json.Marshal(ordinary)
		if err != nil || string(ordinaryJSON) != string(encoded) {
			t.Fatal("benchmark and ordinary production paths drift")
		}
	}
	if model.ProductionRetrievalStrategy != model.StrategyBM25 {
		t.Fatal("production promoted")
	}
}
