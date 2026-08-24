package eval

import (
	"math"
	"sort"
	"strings"

	"repolens/internal/evidence"
	"repolens/internal/retrieval"
)

type Metrics struct {
	FileHitAt5           float64 `json:"file_hit_at_5"`
	FileHitAt10          float64 `json:"file_hit_at_10"`
	MRR                  float64 `json:"mrr"`
	CitationValidityRate float64 `json:"citation_validity_rate"`
	RootCauseSuccessRate float64 `json:"root_cause_success_rate"`
	ForbiddenClaimRate   float64 `json:"forbidden_claim_rate"`
	P50LatencyMs         int64   `json:"p50_latency_ms"`
	P95LatencyMs         int64   `json:"p95_latency_ms"`
	AvgPromptTokens      float64 `json:"avg_prompt_tokens"`
	AvgCompletionTokens  float64 `json:"avg_completion_tokens"`
}

type CaseEvalResult struct {
	CaseID           string
	HitAt5           bool
	HitAt10          bool
	ReciprocalRank   float64
	CitationsTotal   int
	CitationsValid   int
	RootCauseSuccess bool
	ForbiddenClaim   bool
	LatencyMs        int64
	PromptTokens     int
	CompletionTokens int
}

func CalculateRetrievalMetrics(results []retrieval.SearchResult, relevantFiles []string) (hit5, hit10 bool, rr float64) {
	relMap := make(map[string]bool)
	for _, f := range relevantFiles {
		relMap[strings.ToLower(filepathNormalize(f))] = true
	}

	firstMatchRank := 0
	for i, res := range results {
		resPath := strings.ToLower(filepathNormalize(res.Path))
		if relMap[resPath] {
			if firstMatchRank == 0 {
				firstMatchRank = i + 1
			}
			if i < 5 {
				hit5 = true
			}
			if i < 10 {
				hit10 = true
			}
		}
	}

	if firstMatchRank > 0 {
		rr = 1.0 / float64(firstMatchRank)
	}
	return hit5, hit10, rr
}

func CalculateCaseDiagnosis(report *evidence.DiagnosisReportData, evalCase EvalCase) (rootCauseSuccess, forbiddenClaim bool) {
	if report == nil {
		return false, false
	}

	rcLower := strings.ToLower(report.RootCause + " " + report.Summary)

	// Check forbidden claims
	for _, fc := range evalCase.ForbiddenClaims {
		if strings.Contains(rcLower, strings.ToLower(fc)) {
			forbiddenClaim = true
			break
		}
	}

	// Check expected keywords from expected root cause
	expectedTokens := strings.Fields(strings.ToLower(evalCase.ExpectedRootCause))
	matches := 0
	for _, tok := range expectedTokens {
		if len(tok) > 3 && strings.Contains(rcLower, tok) {
			matches++
		}
	}

	if matches >= 2 || (len(expectedTokens) > 0 && float64(matches)/float64(len(expectedTokens)) >= 0.25) {
		rootCauseSuccess = true
	}
	return rootCauseSuccess, forbiddenClaim
}

func AggregateMetrics(results []CaseEvalResult) Metrics {
	n := float64(len(results))
	if n == 0 {
		return Metrics{}
	}

	hit5Count := 0
	hit10Count := 0
	mrrSum := 0.0
	citationsTotal := 0
	citationsValid := 0
	rcSuccessCount := 0
	forbiddenCount := 0
	totalPromptTokens := 0
	totalCompletionTokens := 0
	latencies := make([]int64, len(results))

	for i, r := range results {
		if r.HitAt5 {
			hit5Count++
		}
		if r.HitAt10 {
			hit10Count++
		}
		mrrSum += r.ReciprocalRank
		citationsTotal += r.CitationsTotal
		citationsValid += r.CitationsValid
		if r.RootCauseSuccess {
			rcSuccessCount++
		}
		if r.ForbiddenClaim {
			forbiddenCount++
		}
		totalPromptTokens += r.PromptTokens
		totalCompletionTokens += r.CompletionTokens
		latencies[i] = r.LatencyMs
	}

	sort.Slice(latencies, func(i, j int) bool {
		return latencies[i] < latencies[j]
	})

	p50Idx := int(math.Floor(0.50 * n))
	p95Idx := int(math.Floor(0.95 * n))
	if p50Idx >= len(latencies) {
		p50Idx = len(latencies) - 1
	}
	if p95Idx >= len(latencies) {
		p95Idx = len(latencies) - 1
	}

	citationRate := 1.0
	if citationsTotal > 0 {
		citationRate = float64(citationsValid) / float64(citationsTotal)
	}

	return Metrics{
		FileHitAt5:           float64(hit5Count) / n,
		FileHitAt10:          float64(hit10Count) / n,
		MRR:                  mrrSum / n,
		CitationValidityRate: citationRate,
		RootCauseSuccessRate: float64(rcSuccessCount) / n,
		ForbiddenClaimRate:   float64(forbiddenCount) / n,
		P50LatencyMs:         latencies[p50Idx],
		P95LatencyMs:         latencies[p95Idx],
		AvgPromptTokens:      float64(totalPromptTokens) / n,
		AvgCompletionTokens:  float64(totalCompletionTokens) / n,
	}
}

func filepathNormalize(p string) string {
	return strings.ReplaceAll(p, "\\", "/")
}
