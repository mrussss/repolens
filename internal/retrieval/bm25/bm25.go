package bm25

import (
	"encoding/json"
	"io"
	"math"
	"sort"

	"repolens/internal/retrieval/tokenizer"
)

// Document represents an indexable unit of code (symbol excerpt or chunk).
type Document struct {
	ID            int    `json:"id"`
	FilePath      string `json:"file_path"`
	StartLine     int    `json:"start_line"`
	EndLine       int    `json:"end_line"`
	Content       string `json:"content"`
	SymbolKeyHash string `json:"symbol_key_hash,omitempty"`
	SymbolName    string `json:"symbol_name,omitempty"`
	Kind          string `json:"kind,omitempty"`
	DocLength     int    `json:"doc_length"`
}

// Posting stores term frequency within a document.
type Posting struct {
	DocID    int `json:"doc_id"`
	TermFreq int `json:"term_freq"`
}

// SearchResult represents a scored document match.
type SearchResult struct {
	Document Document `json:"document"`
	Score    float64  `json:"score"`
	Rank     int      `json:"rank"`
}

// Index is an in-memory pure Go BM25 index.
type Index struct {
	K1            float64              `json:"k1"`
	B             float64              `json:"b"`
	Documents     []Document           `json:"documents"`
	InvertedIndex map[string][]Posting `json:"inverted_index"`
	DocFreq       map[string]int       `json:"doc_freq"`
	TotalDocs     int                  `json:"total_docs"`
	AvgDocLength  float64              `json:"avg_doc_length"`
	tokenizer     *tokenizer.CodeTokenizer
}

// NewIndex constructs a new BM25 index with tunable k1 and b parameters.
func NewIndex(k1, b float64) *Index {
	if k1 <= 0 {
		k1 = 1.2
	}
	if b <= 0 {
		b = 0.75
	}
	return &Index{
		K1:            k1,
		B:             b,
		Documents:     make([]Document, 0),
		InvertedIndex: make(map[string][]Posting),
		DocFreq:       make(map[string]int),
		tokenizer:     tokenizer.New(),
	}
}

// AddDocument tokenizes and stages a document into the index.
func (idx *Index) AddDocument(doc Document) {
	tokens := idx.tokenizer.Tokenize(doc.Content + " " + doc.SymbolName + " " + doc.FilePath)
	doc.DocLength = len(tokens)
	doc.ID = len(idx.Documents)
	idx.Documents = append(idx.Documents, doc)

	// Count term frequencies
	tf := make(map[string]int)
	for _, tok := range tokens {
		tf[tok]++
	}

	for term, freq := range tf {
		idx.InvertedIndex[term] = append(idx.InvertedIndex[term], Posting{
			DocID:    doc.ID,
			TermFreq: freq,
		})
		idx.DocFreq[term]++
	}
}

// Build finalizes the statistics (total docs and average document length).
func (idx *Index) Build() {
	idx.TotalDocs = len(idx.Documents)
	if idx.TotalDocs == 0 {
		idx.AvgDocLength = 0
		return
	}
	totalLen := 0
	for _, d := range idx.Documents {
		totalLen += d.DocLength
	}
	idx.AvgDocLength = float64(totalLen) / float64(idx.TotalDocs)
}

// Search performs BM25 scoring for the given query and returns the topK ranked results.
func (idx *Index) Search(query string, topK int) []SearchResult {
	if topK <= 0 {
		topK = 20
	}
	if idx.TotalDocs == 0 || query == "" {
		return nil
	}

	queryTokens := idx.tokenizer.Tokenize(query)
	if len(queryTokens) == 0 {
		return nil
	}

	scores := make(map[int]float64)

	for _, qTerm := range queryTokens {
		postings, exists := idx.InvertedIndex[qTerm]
		if !exists {
			continue
		}

		df := float64(idx.DocFreq[qTerm])
		// Standard Robertson-Spärck Jones IDF with smoothing
		idf := math.Log(1.0 + (float64(idx.TotalDocs)-df+0.5)/(df+0.5))
		if idf < 0 {
			idf = 0.0001
		}

		for _, p := range postings {
			docLen := float64(idx.Documents[p.DocID].DocLength)
			tf := float64(p.TermFreq)

			// BM25 term weighting formula
			numerator := tf * (idx.K1 + 1.0)
			denominator := tf + idx.K1*(1.0-idx.B+idx.B*(docLen/idx.AvgDocLength))
			score := idf * (numerator / denominator)

			// Boost if the query term matches symbol name exactly
			if idx.Documents[p.DocID].SymbolName == qTerm {
				score *= 1.5
			}

			scores[p.DocID] += score
		}
	}

	var results []SearchResult
	for docID, score := range scores {
		if score > 0 {
			results = append(results, SearchResult{
				Document: idx.Documents[docID],
				Score:    score,
			})
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Score == results[j].Score {
			return results[i].Document.ID < results[j].Document.ID
		}
		return results[i].Score > results[j].Score
	})

	if len(results) > topK {
		results = results[:topK]
	}

	for i := range results {
		results[i].Rank = i + 1
	}

	return results
}

// Save serializes the index to JSON stream.
func (idx *Index) Save(w io.Writer) error {
	enc := json.NewEncoder(w)
	return enc.Encode(idx)
}

// Load deserializes the index from JSON stream.
func Load(r io.Reader) (*Index, error) {
	var idx Index
	dec := json.NewDecoder(r)
	if err := dec.Decode(&idx); err != nil {
		return nil, err
	}
	idx.tokenizer = tokenizer.New()
	return &idx, nil
}
