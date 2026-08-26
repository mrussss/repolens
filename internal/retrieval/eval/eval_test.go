package eval_test

import (
	"context"
	"testing"

	"repolens/internal/retrieval/bm25"
	"repolens/internal/retrieval/eval"
	"repolens/internal/retrieval/structural"
)

func TestBenchmarkRunner_EvaluateAndPromotionRule(t *testing.T) {
	idx := bm25.NewIndex(1.2, 0.75)

	idx.AddDocument(bm25.Document{
		FilePath:   "internal/auth/token.go",
		StartLine:  1,
		EndLine:    20,
		Content:    "func ValidateToken(raw string) (*Claims, error)",
		SymbolName: "ValidateToken",
	})
	idx.AddDocument(bm25.Document{
		FilePath:   "internal/payment/charge.go",
		StartLine:  1,
		EndLine:    30,
		Content:    "func ProcessPayment(amount int) error",
		SymbolName: "ProcessPayment",
	})
	idx.AddDocument(bm25.Document{
		FilePath:   "internal/auth/token_test.go",
		StartLine:  1,
		EndLine:    25,
		Content:    "func TestValidateToken(t *testing.T)",
		SymbolName: "TestValidateToken",
	})
	idx.Build()

	testCases := []eval.TestCase{
		{
			ID:             "case-1",
			Query:          "ValidateToken",
			ExpectedSymbol: "ValidateToken",
			ExpectedFiles:  []string{"internal/auth/token.go"},
		},
		{
			ID:             "case-2",
			Query:          "ProcessPayment",
			ExpectedSymbol: "ProcessPayment",
			ExpectedFiles:  []string{"internal/payment/charge.go"},
		},
	}

	runner := eval.NewBenchmarkRunner()
	cMetrics := runner.EvaluateBM25(idx, testCases)

	if cMetrics.HitAt1 != 1.0 {
		t.Errorf("expected Hit@1 = 1.0, got %f", cMetrics.HitAt1)
	}

	structEngine := structural.NewEngine(idx, nil, 0)
	dMetrics := runner.EvaluateStructural(context.Background(), structEngine, testCases)

	if dMetrics.HitAt1 != 1.0 {
		t.Errorf("expected Hit@1 = 1.0, got %f", dMetrics.HitAt1)
	}

	// Check Promotion Rule
	promoRes := eval.CheckPromotionRule(cMetrics, dMetrics)
	if promoRes.WinningStrategy == "" {
		t.Errorf("expected winning strategy set")
	}
}
