package structural

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"repolens/internal/codeintel/model"
	"repolens/internal/retrieval/bm25"
)

type expansionFixtureStore struct {
	rows  map[string][]model.StructuralExpansionCandidate
	calls []string
	err   error
}

func (s *expansionFixtureStore) ListStructuralExpansionCandidates(_ context.Context, _ int64, hash string) (model.StructuralExpansionCandidates, error) {
	s.calls = append(s.calls, hash)
	return model.StructuralExpansionCandidates{Candidates: append([]model.StructuralExpansionCandidate(nil), s.rows[hash]...), SymbolQueries: 1, RelationQueries: 2, RelationsExamined: len(s.rows[hash])}, s.err
}

func expansionFixture() (*bm25.Index, *expansionFixtureStore, []model.Symbol) {
	idx := bm25.NewIndex(1.2, .75)
	for i := 0; i < 16; i++ {
		name := fmt.Sprintf("seed%02d", i)
		idx.AddDocument(bm25.Document{FilePath: name + ".go", SymbolName: name, SymbolKeyHash: name, Content: "needle", StartLine: 1, EndLine: 1})
	}
	var targets []model.Symbol
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("target%02d", i)
		sym := model.Symbol{ID: int64(i + 100), CodeIndexBuildID: 1, SymbolKeyHash: name, SymbolKeyRaw: "raw:" + name, Name: name, Kind: model.SymbolKindFunction, FilePath: name + ".go", StartLine: 1, EndLine: 1}
		targets = append(targets, sym)
		idx.AddDocument(bm25.Document{FilePath: sym.FilePath, SymbolName: name, SymbolKeyHash: name, Content: "unrelated", StartLine: 1, EndLine: 1})
	}
	idx.Build()
	return idx, &expansionFixtureStore{rows: make(map[string][]model.StructuralExpansionCandidate)}, targets
}

func callCandidate(sym model.Symbol) model.StructuralExpansionCandidate {
	return model.StructuralExpansionCandidate{Symbol: sym, RelationType: model.RelationTypeCallCandidate, Direction: model.ExpansionForward, ResolutionKind: model.ResolutionKindSemantic, Confidence: 1, ReasonCode: "SEMANTIC_DIRECT_FUNC"}
}

func TestExpansionPlacementTraceAndNoV1Boosts(t *testing.T) {
	idx, store, targets := expansionFixture()
	base := idx.Search("needle", 16)
	store.rows[base[0].Document.SymbolKeyHash] = []model.StructuralExpansionCandidate{callCandidate(targets[0])}
	results, trace, err := NewExpansionEngine(idx, store, 1).Search(context.Background(), "needle", 8)
	if err != nil || len(results) != 8 || trace.ExpandedCandidates != 1 || trace.Depth != 1 {
		t.Fatalf("results=%+v trace=%+v err=%v", results, trace, err)
	}
	// Base rank 2 wins the tie with the rank-1 seed's new neighbor.
	hit := results[2]
	if hit.Document.SymbolKeyHash != targets[0].SymbolKeyHash || !hit.Trace.Expanded || hit.Trace.EffectiveRank != 2 || hit.Trace.FinalRank != 3 || hit.Trace.BaseBM25Rank != 0 || hit.BaseScore != 0 {
		t.Fatalf("wrong rank placement: %+v", hit)
	}
	r := hit.Trace.BestSeed
	if r == nil || r.Reason != SemanticCallExpansion || r.SeedBM25Rank != 1 || r.SeedSymbolKeyHash != base[0].Document.SymbolKeyHash || len(hit.Trace.Reasons) != 1 {
		t.Fatalf("incomplete typed reason: %+v", hit.Trace)
	}
	if results[0].BaseScore != base[0].Score || results[0].Trace.Expanded {
		t.Fatal("base score changed or V1 boost applied")
	}
}

func TestExpansionRelationAndIdentityGuards(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*model.StructuralExpansionCandidate)
		want bool
	}{
		{"forward semantic", func(c *model.StructuralExpansionCandidate) {}, true},
		{"confidence 0.95", func(c *model.StructuralExpansionCandidate) { c.Confidence = .95 }, true},
		{"confidence 0.94", func(c *model.StructuralExpansionCandidate) { c.Confidence = .94 }, false},
		{"reverse call", func(c *model.StructuralExpansionCandidate) { c.Direction = model.ExpansionReverse }, false},
		{"reference", func(c *model.StructuralExpansionCandidate) { c.RelationType = model.RelationTypeReference }, false},
		{"syntactic", func(c *model.StructuralExpansionCandidate) { c.ResolutionKind = model.ResolutionKindSyntactic }, false},
		{"heuristic", func(c *model.StructuralExpansionCandidate) { c.ResolutionKind = model.ResolutionKindHeuristic }, false},
		{"unresolved", func(c *model.StructuralExpansionCandidate) { c.ResolutionKind = model.ResolutionKindUnresolved }, false},
		{"direct test", func(c *model.StructuralExpansionCandidate) {
			c.RelationType = model.RelationTypeTestRelation
			c.Direction = model.ExpansionReverse
			c.ReasonCode = string(model.TestReasonDirectSemantic)
		}, true},
		{"weak test syntactic", func(c *model.StructuralExpansionCandidate) {
			c.RelationType = model.RelationTypeTestRelation
			c.Direction = model.ExpansionReverse
			c.ReasonCode = string(model.TestReasonDirectSyntactic)
		}, false},
		{"weak test name", func(c *model.StructuralExpansionCandidate) {
			c.RelationType = model.RelationTypeTestRelation
			c.Direction = model.ExpansionReverse
			c.ReasonCode = string(model.TestReasonNameMatch)
		}, false},
		{"weak test package", func(c *model.StructuralExpansionCandidate) {
			c.RelationType = model.RelationTypeTestRelation
			c.Direction = model.ExpansionReverse
			c.ReasonCode = string(model.TestReasonSamePackage)
		}, false},
		{"forward test", func(c *model.StructuralExpansionCandidate) {
			c.RelationType = model.RelationTypeTestRelation
			c.ReasonCode = string(model.TestReasonDirectSemantic)
		}, false},
		{"other build", func(c *model.StructuralExpansionCandidate) { c.Symbol.CodeIndexBuildID = 2 }, false},
		{"missing hash", func(c *model.StructuralExpansionCandidate) { c.Symbol.SymbolKeyHash = "" }, false},
		{"missing raw key", func(c *model.StructuralExpansionCandidate) { c.Symbol.SymbolKeyRaw = "" }, false},
		{"unresolved id", func(c *model.StructuralExpansionCandidate) { c.Symbol.ID = 0 }, false},
		{"outside artifact", func(c *model.StructuralExpansionCandidate) { c.Symbol.SymbolKeyHash = "absent" }, false},
		{"mismatched artifact", func(c *model.StructuralExpansionCandidate) { c.Symbol.EndLine = 2 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idx, store, targets := expansionFixture()
			c := callCandidate(targets[0])
			tc.edit(&c)
			store.rows["seed00"] = []model.StructuralExpansionCandidate{c}
			_, trace, err := NewExpansionEngine(idx, store, 1).Search(context.Background(), "needle", 8)
			if err != nil || (trace.ExpandedCandidates == 1) != tc.want {
				t.Fatalf("trace=%+v err=%v", trace, err)
			}
			if tc.want && c.RelationType == model.RelationTypeTestRelation && trace.Candidates[2].BestSeed.Reason != DirectTestExpansion {
				t.Fatal("test expansion reason missing")
			}
		})
	}
}

func TestExpansionBudgetsDedupAllReasonsAndDeterminism(t *testing.T) {
	idx, store, targets := expansionFixture()
	for i := 0; i < 16; i++ {
		store.rows[fmt.Sprintf("seed%02d", i)] = []model.StructuralExpansionCandidate{callCandidate(targets[i%12]), callCandidate(targets[(i+1)%12])}
	}
	// Two independent seeds point to one new implementation. Duplicates and
	// the reverse direct-test provenance must be retained without another slot.
	store.rows["seed01"] = []model.StructuralExpansionCandidate{callCandidate(targets[0]), callCandidate(targets[0]), callCandidate(targets[2])}
	test := callCandidate(targets[0])
	test.RelationType = model.RelationTypeTestRelation
	test.Direction = model.ExpansionReverse
	test.ReasonCode = string(model.TestReasonDirectSemantic)
	store.rows["seed01"] = append(store.rows["seed01"], test)
	store.rows[targets[0].SymbolKeyHash] = []model.StructuralExpansionCandidate{callCandidate(targets[11])}
	engine := NewExpansionEngine(idx, store, 1)
	var previous []byte
	for n := 0; n < 5; n++ {
		store.calls = nil
		results, trace, err := engine.Search(context.Background(), "needle", 8)
		if err != nil || len(results) != 8 || trace.SeedCount != 8 || len(store.calls) != 8 || trace.ExpandedCandidates != 4 || trace.RelationQueries != 16 || trace.Depth != 1 || len(trace.Candidates) != 20 {
			t.Fatalf("unbounded trace=%+v err=%v", trace, err)
		}
		contribution := map[string]int{}
		for _, hit := range trace.Candidates {
			if !hit.Expanded {
				continue
			}
			contribution[hit.BestSeed.SeedSymbolKeyHash]++
			if hit.SymbolKeyHash == targets[0].SymbolKeyHash && (len(hit.Reasons) != 3 || hit.BestSeed.SeedBM25Rank != 1 || hit.EffectiveRank != 2) {
				t.Fatalf("dedup lost provenance/best seed: %+v", hit)
			}
		}
		for _, count := range contribution {
			if count > 1 {
				t.Fatal("per-seed budget exceeded")
			}
		}
		for _, hash := range store.calls {
			if hash == targets[0].SymbolKeyHash {
				t.Fatal("recursive expansion")
			}
		}
		encoded, _ := json.Marshal(trace)
		if n > 0 && string(previous) != string(encoded) {
			t.Fatal("nondeterministic trace or ranking")
		}
		previous = encoded
	}
}

func TestExpansionSkipsBaseNeighborsAndPropagatesErrors(t *testing.T) {
	idx, store, targets := expansionFixture()
	base := idx.Search("needle", 16)
	neighbor := targets[0]
	neighbor.SymbolKeyHash = base[12].Document.SymbolKeyHash
	neighbor.Name = base[12].Document.SymbolName
	neighbor.FilePath = base[12].Document.FilePath
	store.rows["seed00"] = []model.StructuralExpansionCandidate{callCandidate(neighbor)}
	results, trace, err := NewExpansionEngine(idx, store, 1).Search(context.Background(), "needle", 8)
	if err != nil || trace.ExpandedCandidates != 0 {
		t.Fatalf("existing candidate promoted: %+v %v", trace, err)
	}
	for i, hit := range results {
		if hit.Document.ID != base[i].Document.ID || hit.BaseScore != base[i].Score {
			t.Fatal("lexical order changed without expansion")
		}
	}
	store.err = errors.New("store unavailable")
	_, _, err = NewExpansionEngine(idx, store, 1).Search(context.Background(), "needle", 8)
	if !errors.Is(err, store.err) {
		t.Fatal("store failure hidden")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = NewExpansionEngine(idx, store, 1).Search(ctx, "needle", 8)
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation hidden")
	}
	_, empty, err := NewExpansionEngine(idx, store, 1).Search(context.Background(), "", 8)
	if err != nil || !reflect.DeepEqual(empty.Candidates, []ExpansionResultTrace{}) || empty.SeedCount != 0 {
		t.Fatal("empty query did work")
	}
}

func TestExpansionDuplicateSeedIdentityDoesNotMultiplyBudget(t *testing.T) {
	original, store, targets := expansionFixture()
	idx := bm25.NewIndex(1.2, .75)
	idx.AddDocument(original.Documents[0])
	idx.AddDocument(original.Documents[0])
	for _, doc := range original.Documents[1:] {
		idx.AddDocument(doc)
	}
	idx.Build()
	store.rows["seed00"] = []model.StructuralExpansionCandidate{callCandidate(targets[0]), callCandidate(targets[1])}
	_, trace, err := NewExpansionEngine(idx, store, 1).Search(context.Background(), "needle", 8)
	if err != nil || trace.ExpandedCandidates != 1 || trace.SeedCount != 7 {
		t.Fatalf("duplicate seed bypassed budget: %+v %v", trace, err)
	}
	count := 0
	for _, hash := range store.calls {
		if hash == "seed00" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("duplicate seed queried twice")
	}
}
