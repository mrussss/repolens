package structural

import (
	"context"
	"fmt"
	"sort"

	"repolens/internal/codeintel/model"
	"repolens/internal/retrieval/bm25"
)

const (
	SemanticCallExpansion = "SEMANTIC_CALL_EXPANSION"
	DirectTestExpansion   = "DIRECT_TEST_IMPLEMENTATION_EXPANSION"
)

type ExpansionStore interface {
	ListStructuralExpansionCandidates(context.Context, int64, string) (model.StructuralExpansionCandidates, error)
}

type ExpansionReason struct {
	SeedSymbolKeyHash string               `json:"seed_symbol_key_hash"`
	SeedSymbol        string               `json:"seed_symbol"`
	SeedBM25Rank      int                  `json:"seed_bm25_rank"`
	RelationType      model.RelationType   `json:"relation_type"`
	Direction         string               `json:"direction"`
	ResolutionKind    model.ResolutionKind `json:"resolution_kind"`
	Confidence        float64              `json:"confidence"`
	ReasonCode        string               `json:"reason_code"`
	Reason            string               `json:"reason"`
}

type ExpansionResultTrace struct {
	SymbolKeyHash string            `json:"symbol_key_hash"`
	Symbol        string            `json:"symbol"`
	Path          string            `json:"path"`
	BaseBM25Rank  int               `json:"base_bm25_rank,omitempty"`
	Expanded      bool              `json:"expanded"`
	EffectiveRank int               `json:"effective_rank"`
	FinalRank     int               `json:"final_rank"`
	BestSeed      *ExpansionReason  `json:"best_seed,omitempty"`
	Reasons       []ExpansionReason `json:"expansion_reasons,omitempty"`
}

// Counts describe request-local work. RelationsExamined counts bounded rows
// returned by SQL, not the number of rows a database scans internally.
type ExpansionSearchTrace struct {
	Source             string                 `json:"source"`
	Depth              int                    `json:"depth"`
	SeedCount          int                    `json:"seed_count"`
	SymbolQueries      int                    `json:"symbol_queries"`
	RelationQueries    int                    `json:"relation_queries"`
	RelationsExamined  int                    `json:"relations_examined"`
	ExpandedCandidates int                    `json:"expanded_candidates"`
	Candidates         []ExpansionResultTrace `json:"candidates"`
}

type ExpansionResult struct {
	Document  bm25.Document
	BaseScore float64 // Zero for additions; no full-corpus lexical scoring is done.
	Trace     ExpansionResultTrace
}

type ExpansionEngine struct {
	index   *bm25.Index
	store   ExpansionStore
	buildID int64
}

func NewExpansionEngine(index *bm25.Index, store ExpansionStore, buildID int64) *ExpansionEngine {
	return &ExpansionEngine{index: index, store: store, buildID: buildID}
}

// Search performs only V2 expansion and rank placement. V1 boosts are not used.
func (e *ExpansionEngine) Search(ctx context.Context, query string, topK int) ([]ExpansionResult, *ExpansionSearchTrace, error) {
	if topK <= 0 {
		topK = 20
	}
	trace := &ExpansionSearchTrace{Source: "symbol_bm25_structural_v2", Depth: model.StructuralV2Depth, Candidates: []ExpansionResultTrace{}}
	base := e.index.Search(query, topK*2)
	if len(base) == 0 {
		return nil, trace, nil
	}
	// Artifact membership uses the full pinned document table, never Top50/100
	// search. First document ID wins for duplicate SymbolKeys deterministically.
	documents := make(map[string]bm25.Document)
	for _, doc := range e.index.Documents {
		if doc.SymbolKeyHash != "" {
			if _, exists := documents[doc.SymbolKeyHash]; !exists {
				documents[doc.SymbolKeyHash] = doc
			}
		}
	}
	var merged []*ExpansionResult
	known := make(map[string]*ExpansionResult)
	for _, hit := range base {
		if hit.Document.SymbolKeyHash != "" && known[hit.Document.SymbolKeyHash] != nil {
			continue
		}
		result := &ExpansionResult{Document: hit.Document, BaseScore: hit.Score, Trace: ExpansionResultTrace{
			SymbolKeyHash: hit.Document.SymbolKeyHash, Symbol: hit.Document.SymbolName, Path: hit.Document.FilePath,
			BaseBM25Rank: hit.Rank, EffectiveRank: hit.Rank,
		}}
		merged = append(merged, result)
		if hit.Document.SymbolKeyHash != "" {
			known[hit.Document.SymbolKeyHash] = result
		}
	}
	seeds := base
	if len(seeds) > model.StructuralV2SeedBudget {
		seeds = seeds[:model.StructuralV2SeedBudget]
	}
	if e.store != nil && e.buildID > 0 {
		seenSeeds := make(map[string]bool)
		for _, seed := range seeds {
			if err := ctx.Err(); err != nil {
				return nil, trace, err
			}
			if seed.Document.SymbolKeyHash == "" || seenSeeds[seed.Document.SymbolKeyHash] {
				continue
			}
			seenSeeds[seed.Document.SymbolKeyHash] = true
			trace.SeedCount++
			neighbors, err := e.store.ListStructuralExpansionCandidates(ctx, e.buildID, seed.Document.SymbolKeyHash)
			trace.SymbolQueries += neighbors.SymbolQueries
			trace.RelationQueries += neighbors.RelationQueries
			trace.RelationsExamined += neighbors.RelationsExamined
			if err != nil {
				return nil, trace, fmt.Errorf("V2 expansion for seed %s: %w", seed.Document.SymbolKeyHash, err)
			}
			neighbors.Candidates = append([]model.StructuralExpansionCandidate(nil), neighbors.Candidates...)
			// SQL has already limited each class. Keep the same order even for
			// store adapters that return rows in another order.
			sort.Slice(neighbors.Candidates, func(i, j int) bool {
				a, b := neighbors.Candidates[i], neighbors.Candidates[j]
				if relationPriority(a.RelationType) != relationPriority(b.RelationType) {
					return relationPriority(a.RelationType) < relationPriority(b.RelationType)
				}
				if a.Symbol.SymbolKeyHash != b.Symbol.SymbolKeyHash {
					return a.Symbol.SymbolKeyHash < b.Symbol.SymbolKeyHash
				}
				if a.Confidence != b.Confidence {
					return a.Confidence > b.Confidence
				}
				return a.ReasonCode < b.ReasonCode
			})
			newForSeed := 0
			for _, neighbor := range neighbors.Candidates {
				if !validExpansionCandidate(neighbor, e.buildID) {
					continue
				}
				doc, exists := documents[neighbor.Symbol.SymbolKeyHash]
				if !exists || doc.FilePath != neighbor.Symbol.FilePath || doc.SymbolName != neighbor.Symbol.Name || doc.StartLine != neighbor.Symbol.StartLine || doc.EndLine != neighbor.Symbol.EndLine {
					continue
				}
				candidate := known[doc.SymbolKeyHash]
				if candidate != nil && !candidate.Trace.Expanded {
					continue // Existing lexical neighbors receive no placement boost.
				}
				reason := ExpansionReason{SeedSymbolKeyHash: seed.Document.SymbolKeyHash, SeedSymbol: seed.Document.SymbolName,
					SeedBM25Rank: seed.Rank, RelationType: neighbor.RelationType, Direction: neighbor.Direction,
					ResolutionKind: neighbor.ResolutionKind, Confidence: neighbor.Confidence, ReasonCode: neighbor.ReasonCode,
					Reason: SemanticCallExpansion}
				if neighbor.RelationType == model.RelationTypeTestRelation {
					reason.Reason = DirectTestExpansion
				}
				if candidate == nil {
					if newForSeed >= model.StructuralV2PerSeedBudget || trace.ExpandedCandidates >= model.StructuralV2ExpansionBudget {
						continue
					}
					candidate = &ExpansionResult{Document: doc, Trace: ExpansionResultTrace{
						SymbolKeyHash: doc.SymbolKeyHash, Symbol: doc.SymbolName, Path: doc.FilePath,
						Expanded: true, EffectiveRank: seed.Rank + 1, BestSeed: &reason,
					}}
					known[doc.SymbolKeyHash] = candidate
					merged = append(merged, candidate)
					newForSeed++
					trace.ExpandedCandidates++
				}
				if !containsExpansionReason(candidate.Trace.Reasons, reason) {
					candidate.Trace.Reasons = append(candidate.Trace.Reasons, reason)
				}
				if betterExpansionReason(reason, *candidate.Trace.BestSeed) {
					candidate.Trace.BestSeed = &reason
					candidate.Trace.EffectiveRank = seed.Rank + 1
				}
			}
		}
	}
	sort.Slice(merged, func(i, j int) bool {
		a, b := merged[i].Trace, merged[j].Trace
		if a.EffectiveRank != b.EffectiveRank {
			return a.EffectiveRank < b.EffectiveRank
		}
		if a.Expanded != b.Expanded {
			return !a.Expanded
		}
		if a.Expanded && b.Expanded {
			if betterExpansionReason(*a.BestSeed, *b.BestSeed) {
				return true
			}
			if betterExpansionReason(*b.BestSeed, *a.BestSeed) {
				return false
			}
		}
		return a.SymbolKeyHash < b.SymbolKeyHash
	})
	var results []ExpansionResult
	for i, candidate := range merged {
		candidate.Trace.FinalRank = i + 1
		trace.Candidates = append(trace.Candidates, candidate.Trace)
		if i < topK {
			results = append(results, *candidate)
		}
	}
	return results, trace, nil
}

func validExpansionCandidate(c model.StructuralExpansionCandidate, buildID int64) bool {
	s := c.Symbol
	if s.ID <= 0 || s.CodeIndexBuildID != buildID || s.SymbolKeyHash == "" || s.SymbolKeyRaw == "" || s.Name == "" || s.FilePath == "" || s.StartLine < 1 || s.EndLine < s.StartLine || c.ResolutionKind != model.ResolutionKindSemantic || !(c.Confidence >= model.StructuralV2MinConfidence) {
		return false
	}
	return (c.RelationType == model.RelationTypeCallCandidate && c.Direction == model.ExpansionForward) ||
		(c.RelationType == model.RelationTypeTestRelation && c.Direction == model.ExpansionReverse && c.ReasonCode == string(model.TestReasonDirectSemantic))
}

func relationPriority(kind model.RelationType) int {
	if kind == model.RelationTypeCallCandidate {
		return 0
	}
	return 1
}

func betterExpansionReason(a, b ExpansionReason) bool {
	// Placement is authoritative; relation priority resolves equal placement.
	if a.SeedBM25Rank != b.SeedBM25Rank {
		return a.SeedBM25Rank < b.SeedBM25Rank
	}
	return relationPriority(a.RelationType) < relationPriority(b.RelationType)
}

func containsExpansionReason(reasons []ExpansionReason, reason ExpansionReason) bool {
	for _, existing := range reasons {
		if existing == reason {
			return true
		}
	}
	return false
}
