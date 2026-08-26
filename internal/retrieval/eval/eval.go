package eval

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"repolens/internal/retrieval/bm25"
	"repolens/internal/retrieval/structural"
)

// TestCase represents an evaluation sample.
type TestCase struct {
	ID             string   `json:"id"`
	Query          string   `json:"query"`
	ExpectedSymbol string   `json:"expected_symbol"`
	ExpectedFiles  []string `json:"expected_files"`
}

// StrategyMetrics captures benchmark performance of a retrieval strategy.
type StrategyMetrics struct {
	StrategyName   string             `json:"strategy_name"`
	HitAt1         float64            `json:"hit_at_1"`
	HitAt5         float64            `json:"hit_at_5"`
	HitAt5Count    int                `json:"hit_at_5_count"`
	MeanMRR        float64            `json:"mean_mrr"`
	EvidenceRecall float64            `json:"evidence_recall"`
	PerCaseRecall  map[string]float64 `json:"per_case_recall"`
	P95LatencyMs   float64            `json:"p95_latency_ms"`
}

// PromotionResult reports whether Strategy D qualifies for production over Strategy C.
type PromotionResult struct {
	PromotedToProduction bool     `json:"promoted_to_production"`
	WinningStrategy      string   `json:"winning_strategy"`
	RuleViolations       []string `json:"rule_violations"`
	Summary              string   `json:"summary"`
}

// BenchmarkRunner evaluates retrieval strategies over a test set.
type BenchmarkRunner struct{}

func NewBenchmarkRunner() *BenchmarkRunner {
	return &BenchmarkRunner{}
}

// EvaluateBM25 runs benchmarks for Strategy C (Pure Go BM25).
func (r *BenchmarkRunner) EvaluateBM25(idx *bm25.Index, testCases []TestCase) StrategyMetrics {
	var latencies []float64
	hit1 := 0
	hit5 := 0
	var reciprocalRanks []float64
	perCaseRecall := make(map[string]float64)
	var recalls []float64

	for _, tc := range testCases {
		start := time.Now()
		results := idx.Search(tc.Query, 10)
		latencies = append(latencies, float64(time.Since(start).Microseconds())/1000.0)

		foundRank := 0
		foundFiles := make(map[string]bool)

		for _, res := range results {
			if tc.ExpectedSymbol != "" && res.Document.SymbolName == tc.ExpectedSymbol {
				if foundRank == 0 {
					foundRank = res.Rank
				}
			}
			for _, expF := range tc.ExpectedFiles {
				if res.Document.FilePath == expF {
					foundFiles[expF] = true
				}
			}
		}

		if foundRank == 1 {
			hit1++
		}
		if foundRank > 0 && foundRank <= 5 {
			hit5++
		}
		if foundRank > 0 {
			reciprocalRanks = append(reciprocalRanks, 1.0/float64(foundRank))
		} else {
			reciprocalRanks = append(reciprocalRanks, 0.0)
		}

		recall := 0.0
		if len(tc.ExpectedFiles) > 0 {
			recall = float64(len(foundFiles)) / float64(len(tc.ExpectedFiles))
		} else if foundRank > 0 && foundRank <= 5 {
			recall = 1.0
		}
		perCaseRecall[tc.ID] = recall
		recalls = append(recalls, recall)
	}

	n := float64(len(testCases))
	if n == 0 {
		return StrategyMetrics{StrategyName: "Symbol BM25"}
	}

	return StrategyMetrics{
		StrategyName:   "Symbol BM25",
		HitAt1:         float64(hit1) / n,
		HitAt5:         float64(hit5) / n,
		HitAt5Count:    hit5,
		MeanMRR:        calcMean(reciprocalRanks),
		EvidenceRecall: calcMean(recalls),
		PerCaseRecall:  perCaseRecall,
		P95LatencyMs:   calcP95(latencies),
	}
}

// EvaluateStructural runs benchmarks for Strategy D (Symbol BM25 + Structural).
func (r *BenchmarkRunner) EvaluateStructural(ctx context.Context, engine *structural.Engine, testCases []TestCase) StrategyMetrics {
	var latencies []float64
	hit1 := 0
	hit5 := 0
	var reciprocalRanks []float64
	perCaseRecall := make(map[string]float64)
	var recalls []float64

	for _, tc := range testCases {
		start := time.Now()
		results := engine.Search(ctx, tc.Query, 10)
		latencies = append(latencies, float64(time.Since(start).Microseconds())/1000.0)

		foundRank := 0
		foundFiles := make(map[string]bool)

		for _, res := range results {
			if tc.ExpectedSymbol != "" && res.Document.SymbolName == tc.ExpectedSymbol {
				if foundRank == 0 {
					foundRank = res.FinalRank
				}
			}
			for _, expF := range tc.ExpectedFiles {
				if res.Document.FilePath == expF {
					foundFiles[expF] = true
				}
			}
		}

		if foundRank == 1 {
			hit1++
		}
		if foundRank > 0 && foundRank <= 5 {
			hit5++
		}
		if foundRank > 0 {
			reciprocalRanks = append(reciprocalRanks, 1.0/float64(foundRank))
		} else {
			reciprocalRanks = append(reciprocalRanks, 0.0)
		}

		recall := 0.0
		if len(tc.ExpectedFiles) > 0 {
			recall = float64(len(foundFiles)) / float64(len(tc.ExpectedFiles))
		} else if foundRank > 0 && foundRank <= 5 {
			recall = 1.0
		}
		perCaseRecall[tc.ID] = recall
		recalls = append(recalls, recall)
	}

	n := float64(len(testCases))
	if n == 0 {
		return StrategyMetrics{StrategyName: "Symbol BM25 + Structural"}
	}

	return StrategyMetrics{
		StrategyName:   "Symbol BM25 + Structural",
		HitAt1:         float64(hit1) / n,
		HitAt5:         float64(hit5) / n,
		HitAt5Count:    hit5,
		MeanMRR:        calcMean(reciprocalRanks),
		EvidenceRecall: calcMean(recalls),
		PerCaseRecall:  perCaseRecall,
		P95LatencyMs:   calcP95(latencies),
	}
}

// CheckPromotionRule verifies whether Strategy D satisfies the frozen promotion rule over Strategy C.
func CheckPromotionRule(cMetrics, dMetrics StrategyMetrics) PromotionResult {
	var violations []string

	// Rule 1: Hit@5 count D >= C
	if dMetrics.HitAt5Count < cMetrics.HitAt5Count {
		violations = append(violations, fmt.Sprintf("Hit@5 count dropped: D=%d < C=%d", dMetrics.HitAt5Count, cMetrics.HitAt5Count))
	}

	// Rule 2: Mean MRR D >= C - 0.01
	if dMetrics.MeanMRR < (cMetrics.MeanMRR - 0.01) {
		violations = append(violations, fmt.Sprintf("Mean MRR degraded: D=%.3f < C=%.3f - 0.01", dMetrics.MeanMRR, cMetrics.MeanMRR))
	}

	// Rule 3: Evidence Recall improvement on at least 2 cases, 0 cases degradation >= 0.10
	improvedCount := 0
	degradedCount := 0
	for tcID, cRecall := range cMetrics.PerCaseRecall {
		dRecall := dMetrics.PerCaseRecall[tcID]
		if dRecall > cRecall+0.001 {
			improvedCount++
		} else if (cRecall - dRecall) >= 0.10 {
			degradedCount++
		}
	}
	if improvedCount < 2 {
		violations = append(violations, fmt.Sprintf("Evidence Recall did not strictly improve on >= 2 cases (improved: %d)", improvedCount))
	}
	if degradedCount > 0 {
		violations = append(violations, fmt.Sprintf("Evidence Recall had %d case(s) with severe degradation (>= 0.10)", degradedCount))
	}

	// Rule 4: P95 Latency D <= 1.5 * C
	if dMetrics.P95LatencyMs > 1.5*math.Max(1.0, cMetrics.P95LatencyMs) {
		violations = append(violations, fmt.Sprintf("P95 latency exceeded threshold: D=%.2fms > 1.5*C (%.2fms)", dMetrics.P95LatencyMs, cMetrics.P95LatencyMs))
	}

	promoted := len(violations) == 0
	winningStrategy := "SYMBOL_BM25"
	if promoted {
		winningStrategy = "SYMBOL_BM25_STRUCTURAL"
	}

	summary := fmt.Sprintf("Promotion Decision: %s (Promoted=%v, Violations=%d)", winningStrategy, promoted, len(violations))
	return PromotionResult{
		PromotedToProduction: promoted,
		WinningStrategy:      winningStrategy,
		RuleViolations:       violations,
		Summary:              summary,
	}
}

func calcMean(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func calcP95(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	idx := int(float64(len(vals)-1) * 0.95)
	return vals[idx]
}
