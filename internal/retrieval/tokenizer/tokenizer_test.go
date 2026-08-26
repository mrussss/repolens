package tokenizer_test

import (
	"slices"
	"testing"

	"repolens/internal/retrieval/tokenizer"
)

func TestCodeTokenizer(t *testing.T) {
	tok := tokenizer.New()

	tests := []struct {
		input    string
		contains []string
	}{
		{
			input:    "ValidateToken",
			contains: []string{"ValidateToken", "validatetoken", "validate", "token"},
		},
		{
			input:    "validate_token",
			contains: []string{"validate_token", "validate", "token"},
		},
		{
			input:    "HTTPServer",
			contains: []string{"HTTPServer", "httpserver", "HTTP", "http", "Server", "server"},
		},
		{
			input:    "order.ProcessOrder(ctx, id)",
			contains: []string{"order", "ProcessOrder", "Process", "Order", "ctx", "id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			tokens := tok.Tokenize(tt.input)
			for _, expected := range tt.contains {
				if !slices.Contains(tokens, expected) {
					t.Errorf("input %q: expected tokens to contain %q, got %v", tt.input, expected, tokens)
				}
			}
		})
	}
}
