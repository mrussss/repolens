package model

import (
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestStructuralV2IdentityPreservesHistoricalStrategies(t *testing.T) {
	legacyBM25 := fmt.Sprintf("%x", sha256.Sum256([]byte("BM25|k1=1.2|b=0.75|structural=none")))
	legacyV1 := fmt.Sprintf("%x", sha256.Sum256([]byte("BM25_STRUCTURAL|k1=1.2|b=0.75|structural=symbol-expansion-v1")))
	if RetrievalConfigHash(StrategyBM25) != legacyBM25 || RetrievalConfigHash(StrategyBM25Structural) != legacyV1 {
		t.Fatal("historical build hash changed")
	}
	if StructuralV2SeedBudget != 8 || StructuralV2PerSeedBudget != 1 || StructuralV2ExpansionBudget != 4 || StructuralV2Depth != 1 || StructuralV2MinConfidence != .95 || StructuralV2RelationLimit != 8 {
		t.Fatal("fixed experimental budgets changed")
	}
	raw := fmt.Sprintf("BM25_STRUCTURAL_V2|k1=1.2|b=0.75|structural=semantic-onehop-v2|base=2*topK|seeds=%d|per_seed=%d|max_expanded=%d|depth=%d|min_confidence=%.2f|rows_per_direction=%d|call=forward-semantic|test=reverse-direct-semantic|order=effective-rank,base-first,call-first,seed-rank,symbol-hash|targets=hash-asc,confidence-desc,reason-asc|base-neighbors=skip", StructuralV2SeedBudget, StructuralV2PerSeedBudget, StructuralV2ExpansionBudget, StructuralV2Depth, StructuralV2MinConfidence, StructuralV2RelationLimit)
	v2 := RetrievalConfigHash(StrategyBM25StructuralV2)
	if v2 != fmt.Sprintf("%x", sha256.Sum256([]byte(raw))) || v2 == legacyBM25 || v2 == legacyV1 || ProductionRetrievalStrategy != StrategyBM25 {
		t.Fatal("V2 identity is incomplete or production promoted")
	}
}
