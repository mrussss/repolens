package bm25_test

import (
	"bytes"
	"testing"

	"repolens/internal/retrieval/bm25"
)

func TestBM25Index_AddSearchAndSerialize(t *testing.T) {
	idx := bm25.NewIndex(1.2, 0.75)

	idx.AddDocument(bm25.Document{
		FilePath:   "pkg/auth/token.go",
		StartLine:  10,
		EndLine:    25,
		Content:    "func ValidateToken(jwtString string) (*Claims, error) { parse jwt }",
		SymbolName: "ValidateToken",
		Kind:       "FUNCTION",
	})

	idx.AddDocument(bm25.Document{
		FilePath:   "pkg/order/process.go",
		StartLine:  30,
		EndLine:    50,
		Content:    "func ProcessOrder(id string) error { validate inventory and charge card }",
		SymbolName: "ProcessOrder",
		Kind:       "FUNCTION",
	})

	idx.AddDocument(bm25.Document{
		FilePath:   "pkg/auth/token_test.go",
		StartLine:  1,
		EndLine:    15,
		Content:    "func TestValidateToken(t *testing.T) { ValidateToken('valid.jwt') }",
		SymbolName: "TestValidateToken",
		Kind:       "FUNCTION",
	})

	idx.Build()

	if idx.TotalDocs != 3 {
		t.Fatalf("expected 3 docs, got %d", idx.TotalDocs)
	}

	// Search for "ValidateToken"
	results := idx.Search("ValidateToken", 5)
	if len(results) == 0 {
		t.Fatalf("expected search results for ValidateToken")
	}

	if results[0].Document.SymbolName != "ValidateToken" && results[0].Document.SymbolName != "TestValidateToken" {
		t.Errorf("expected top result to be auth token related, got %s", results[0].Document.SymbolName)
	}

	// Test Serialization & Deserialization
	var buf bytes.Buffer
	if err := idx.Save(&buf); err != nil {
		t.Fatalf("failed serializing index: %v", err)
	}

	loadedIdx, err := bm25.Load(&buf)
	if err != nil {
		t.Fatalf("failed deserializing index: %v", err)
	}

	loadedResults := loadedIdx.Search("ProcessOrder", 5)
	if len(loadedResults) == 0 || loadedResults[0].Document.SymbolName != "ProcessOrder" {
		t.Fatalf("expected loaded index to find ProcessOrder")
	}
}
