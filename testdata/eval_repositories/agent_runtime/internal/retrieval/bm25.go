package retrieval

import (
	"context"
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"repolens/internal/indexing"
)

var (
	tokenSplitRegex = regexp.MustCompile(`[_\-\.\:\/\s\(\)\[\]\{\}\,\;\=\<\>\!\+\*\&\|\^\%\$\#\@\"\'\?\~\\\` + "`" + `]+`)
)

func TokenizeCode(text string) []string {
	rawTokens := tokenSplitRegex.Split(text, -1)
	var tokens []string
	for _, tok := range rawTokens {
		if tok == "" {
			continue
		}
		// Split camelCase
		subTokens := splitCamelCase(tok)
		for _, st := range subTokens {
			stLower := strings.ToLower(st)
			if len(stLower) >= 2 {
				tokens = append(tokens, stLower)
			}
		}
	}
	return tokens
}

func splitCamelCase(s string) []string {
	var words []string
	var current []rune
	runes := []rune(s)

	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if unicode.IsUpper(r) {
			if len(current) > 0 {
				// check if previous was lower or next is lower
				words = append(words, string(current))
				current = []rune{r}
			} else {
				current = append(current, r)
			}
		} else {
			current = append(current, r)
		}
	}
	if len(current) > 0 {
		words = append(words, string(current))
	}
	return words
}

type BM25Retriever struct {
	chunkStore ChunkIndexStore
	k1         float64
	b          float64
}

func NewBM25Retriever(chunkStore ChunkIndexStore) *BM25Retriever {
	return &BM25Retriever{
		chunkStore: chunkStore,
		k1:         1.2,
		b:          0.75,
	}
}

func (r *BM25Retriever) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	chunks := r.chunkStore.GetChunks(req.SnapshotID)
	if len(chunks) == 0 {
		return nil, nil
	}

	if req.TopK <= 0 {
		req.TopK = 10
	}

	queryTokens := TokenizeCode(req.Query)
	if len(queryTokens) == 0 {
		return nil, nil
	}

	N := float64(len(chunks))
	docTermFreqs := make([]map[string]int, len(chunks))
	docLengths := make([]float64, len(chunks))
	totalLength := 0.0
	df := make(map[string]int)

	for i, c := range chunks {
		text := c.Content + " " + c.Path + " " + c.Symbol
		tokens := TokenizeCode(text)
		docLengths[i] = float64(len(tokens))
		totalLength += docLengths[i]

		tf := make(map[string]int)
		seenInDoc := make(map[string]bool)
		for _, tok := range tokens {
			tf[tok]++
			if !seenInDoc[tok] {
				seenInDoc[tok] = true
				df[tok]++
			}
		}
		docTermFreqs[i] = tf
	}

	avgdl := totalLength / N
	if avgdl == 0 {
		avgdl = 1
	}

	// Calculate IDF for query tokens
	idf := make(map[string]float64)
	for _, q := range queryTokens {
		n_q := float64(df[q])
		// Okapi BM25 IDF
		idfVal := math.Log(((N - n_q + 0.5) / (n_q + 0.5)) + 1.0)
		if idfVal < 0 {
			idfVal = 0.01
		}
		idf[q] = idfVal
	}

	type scoredResult struct {
		chunk indexing.CodeChunk
		score float64
	}

	var candidates []scoredResult

	for i, c := range chunks {
		if req.Scope != "" && !strings.HasPrefix(c.Path, req.Scope) {
			continue
		}

		score := 0.0
		tf := docTermFreqs[i]
		docLen := docLengths[i]

		for _, q := range queryTokens {
			freq := float64(tf[q])
			if freq == 0 {
				continue
			}
			numerator := freq * (r.k1 + 1.0)
			denominator := freq + r.k1*(1.0-r.b+r.b*(docLen/avgdl))
			score += idf[q] * (numerator / denominator)
		}

		// Boost symbol exact matches
		if c.Symbol != "" && strings.Contains(strings.ToLower(c.Symbol), strings.ToLower(req.Query)) {
			score += 3.0
		}

		if score > 0 {
			candidates = append(candidates, scoredResult{
				chunk: c,
				score: score,
			})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].score > candidates[j].score
	})

	limit := req.TopK
	if len(candidates) < limit {
		limit = len(candidates)
	}

	results := make([]SearchResult, limit)
	for i := 0; i < limit; i++ {
		c := candidates[i].chunk
		results[i] = SearchResult{
			ChunkID:         c.ID,
			Path:            c.Path,
			Language:        c.Language,
			Symbol:          c.Symbol,
			StartLine:       c.StartLine,
			EndLine:         c.EndLine,
			Snippet:         c.Content,
			Score:           candidates[i].score,
			RetrievalSource: "bm25",
		}
	}

	return results, nil
}
