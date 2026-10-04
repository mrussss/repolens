package eval

import (
	"context"
	"fmt"
	"repolens/internal/retrieval/bm25"
	"repolens/internal/retrieval/structural"
	"testing"
)

func TestFileAndSymbolRelevanceBothStrategies(t *testing.T) {
	idx := bm25.NewIndex(1.2, 0.75)
	for i := 1; i <= 5; i++ {
		idx.AddDocument(bm25.Document{ID: i, FilePath: fmt.Sprintf("file%d.go", i), SymbolName: fmt.Sprintf("Symbol%d", i), Content: "needle"})
	}
	idx.Build()
	ranked := idx.Search("needle", 10)
	cases := []struct {
		name   string
		symbol string
		files  []string
		rank   int
		recall float64
	}{
		{"file first", "", []string{ranked[0].Document.FilePath}, 1, 1},
		{"file third", "", []string{ranked[2].Document.FilePath}, 3, 1},
		{"no relevant", "", []string{"missing.go"}, 0, 0},
		{"multiple files with duplicates", "", []string{ranked[0].Document.FilePath, ranked[0].Document.FilePath, ranked[2].Document.FilePath}, 1, 1},
		{"symbol only", ranked[2].Document.SymbolName, nil, 3, 1},
		{"symbol or supporting file", ranked[2].Document.SymbolName, []string{ranked[0].Document.FilePath}, 1, 1},
		{"symbol wins earlier than file", ranked[0].Document.SymbolName, []string{ranked[2].Document.FilePath}, 1, 1},
		{"symbol missing but file relevant", "Missing", []string{ranked[2].Document.FilePath}, 3, 1},
		{"no ground truth", "", nil, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			samples := []TestCase{{ID: tc.name, Query: "needle", ExpectedSymbol: tc.symbol, ExpectedFiles: tc.files}}
			runner := NewBenchmarkRunner()
			for _, metrics := range []StrategyMetrics{runner.EvaluateBM25(idx, samples), runner.EvaluateStructural(context.Background(), structural.NewEngine(idx, nil, 0), samples)} {
				hit1, hit5, mrr := 0.0, 0.0, 0.0
				if tc.rank == 1 {
					hit1 = 1
				}
				if tc.rank > 0 && tc.rank <= 5 {
					hit5 = 1
				}
				if tc.rank > 0 {
					mrr = 1.0 / float64(tc.rank)
				}
				if metrics.HitAt1 != hit1 || metrics.HitAt5 != hit5 || metrics.MeanMRR != mrr || metrics.EvidenceRecall != tc.recall {
					t.Fatalf("%s: %+v; rank=%d recall=%f", metrics.StrategyName, metrics, tc.rank, tc.recall)
				}
			}
		})
	}
}

func TestRelevanceKeepsEarliestRankRegardlessOfObservationOrder(t *testing.T) {
	r := newRelevance(TestCase{ExpectedFiles: []string{"foo.go", "bar.go", "foo.go"}})
	r.observe(bm25.Document{FilePath: "foo.go"}, 3)
	r.observe(bm25.Document{FilePath: "bar.go"}, 1)
	r.observe(bm25.Document{FilePath: "foo.go"}, 5)
	if r.rank != 1 || r.recall() != 1 {
		t.Fatalf("relevance=%+v recall=%f", r, r.recall())
	}
}
