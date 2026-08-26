package tokenizer

import (
	"strings"
	"unicode"
)

// CodeTokenizer splits code text and identifiers into search tokens while preserving original identifiers.
type CodeTokenizer struct{}

func New() *CodeTokenizer {
	return &CodeTokenizer{}
}

// Tokenize processes a code string or query into a list of tokens.
// It preserves original identifier forms as well as their camelCase and snake_case subwords in lowercase.
func (t *CodeTokenizer) Tokenize(text string) []string {
	if text == "" {
		return nil
	}

	rawWords := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '.' && r != ':' && r != '/' && r != '-'
	})

	seen := make(map[string]struct{})
	var tokens []string

	addToken := func(tok string) {
		tok = strings.TrimSpace(tok)
		if len(tok) == 0 {
			return
		}
		if _, exists := seen[tok]; !exists {
			seen[tok] = struct{}{}
			tokens = append(tokens, tok)
		}
	}

	for _, w := range rawWords {
		// 1. Add exact raw token
		addToken(w)

		// 2. Add lowercased full token
		wLower := strings.ToLower(w)
		addToken(wLower)

		// 3. Split on dots, colons, slashes, dashes, underscores
		subParts := strings.FieldsFunc(w, func(r rune) bool {
			return r == '.' || r == ':' || r == '/' || r == '-' || r == '_'
		})
		for _, sp := range subParts {
			addToken(sp)
			addToken(strings.ToLower(sp))

			// 4. Split CamelCase within each subpart
			camelParts := splitCamelCase(sp)
			for _, cp := range camelParts {
				addToken(cp)
				addToken(strings.ToLower(cp))
			}
		}
	}

	return tokens
}

// splitCamelCase splits words like "HTTPServer" -> ["HTTP", "Server"], "ProcessOrder" -> ["Process", "Order"].
func splitCamelCase(s string) []string {
	var parts []string
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}

	start := 0
	for i := 0; i < len(runes); i++ {
		// Case 1: Transition from lowercase/digit to uppercase: "fooBar" -> "foo", "Bar"
		if i > 0 && unicode.IsUpper(runes[i]) && (unicode.IsLower(runes[i-1]) || unicode.IsDigit(runes[i-1])) {
			parts = append(parts, string(runes[start:i]))
			start = i
		}
		// Case 2: Transition from uppercase acronym to lowercase: "HTTPServer" -> "HTTP", "Server"
		if i > 1 && unicode.IsUpper(runes[i-1]) && unicode.IsLower(runes[i]) && unicode.IsUpper(runes[i-2]) {
			parts = append(parts, string(runes[start:i-1]))
			start = i - 1
		}
	}
	if start < len(runes) {
		parts = append(parts, string(runes[start:]))
	}
	return parts
}
