package structural

import (
	"context"
	"sort"
	"strings"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/retrieval/bm25"
)

// StructuralResult represents a search result enhanced by code intelligence graph signals.
type StructuralResult struct {
	Document          bm25.Document `json:"document"`
	BaseScore         float64       `json:"base_score"`
	BaseRank          int           `json:"base_rank"`
	StructuralReasons []string      `json:"structural_reasons"`
	FinalScore        float64       `json:"final_score"`
	FinalRank         int           `json:"final_rank"`
}

// Engine implements explainable structural code retrieval.
type Engine struct {
	bm25Index *bm25.Index
	ciStore   codeintelstore.Store
	buildID   int64
}

// NewEngine constructs a new Structural Retrieval Engine.
func NewEngine(bm25Index *bm25.Index, ciStore codeintelstore.Store, buildID int64) *Engine {
	return &Engine{
		bm25Index: bm25Index,
		ciStore:   ciStore,
		buildID:   buildID,
	}
}

// Search performs BM25 retrieval followed by structural expansion and boosting.
func (e *Engine) Search(ctx context.Context, query string, topK int) []StructuralResult {
	if topK <= 0 {
		topK = 20
	}
	baseResults := e.bm25Index.Search(query, topK*2)
	if len(baseResults) == 0 {
		return nil
	}

	queryLower := strings.ToLower(query)
	var structured []StructuralResult

	for _, br := range baseResults {
		score := br.Score
		var reasons []string

		// Signal 1: Exact Symbol Match
		if br.Document.SymbolName != "" && strings.EqualFold(br.Document.SymbolName, query) {
			score += 0.5
			reasons = append(reasons, "EXACT_SYMBOL_MATCH")
		} else if br.Document.SymbolName != "" && strings.Contains(queryLower, strings.ToLower(br.Document.SymbolName)) {
			score += 0.25
			reasons = append(reasons, "SUBSTRING_SYMBOL_MATCH")
		}

		// Signal 2: Structural Relations (References, Call Candidates, Related Tests)
		if e.ciStore != nil && e.buildID > 0 && br.Document.SymbolKeyHash != "" {
			tests, err := e.ciStore.ListRelatedTests(ctx, e.buildID, br.Document.SymbolKeyHash)
			if err == nil && len(tests) > 0 {
				score += 0.3
				reasons = append(reasons, "RELATED_TEST_DISCOVERY")
			}
		}

		// Signal 3: Test file association
		if strings.HasSuffix(br.Document.FilePath, "_test.go") {
			if strings.Contains(queryLower, "test") || strings.Contains(queryLower, "error") {
				score += 0.2
				reasons = append(reasons, "TEST_CONTEXT_MATCH")
			}
		}

		structured = append(structured, StructuralResult{
			Document:          br.Document,
			BaseScore:         br.Score,
			BaseRank:          br.Rank,
			StructuralReasons: reasons,
			FinalScore:        score,
		})
	}

	// Re-rank by final score
	sort.Slice(structured, func(i, j int) bool {
		if structured[i].FinalScore == structured[j].FinalScore {
			return structured[i].BaseRank < structured[j].BaseRank
		}
		return structured[i].FinalScore > structured[j].FinalScore
	})

	if len(structured) > topK {
		structured = structured[:topK]
	}

	for i := range structured {
		structured[i].FinalRank = i + 1
	}

	return structured
}

// ConvertResultsToCodeChunks converts BM25 or Structural results to generic chunks for agent consumption.
func ConvertResultsToCodeChunks(results []StructuralResult) []codeintelmodel.CodeFile {
	var files []codeintelmodel.CodeFile
	for _, r := range results {
		files = append(files, codeintelmodel.CodeFile{
			Path:        r.Document.FilePath,
			LineCount:   r.Document.EndLine - r.Document.StartLine + 1,
			ParseStatus: "OK",
		})
	}
	return files
}
