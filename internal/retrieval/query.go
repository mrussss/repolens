package retrieval

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

var stackTokenPattern = regexp.MustCompile(`(?:[A-Za-z0-9_./-]+\.(?:go|rs|py|ts|js)|[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*|(?:panic|error|fatal|exception|timeout|deadlock|nil))`)

// BuildQuery extracts a bounded, deterministic retrieval query. In
// particular, it never forwards the complete CI log to BM25 or the model.
func BuildQuery(issueTitle, issueDescription, errorLog string) string {
	parts := []string{strings.TrimSpace(issueTitle)}
	if description := strings.TrimSpace(issueDescription); description != "" {
		parts = append(parts, boundedWords(description, 24))
	}
	matches := stackTokenPattern.FindAllString(errorLog, -1)
	seen := make(map[string]struct{})
	for _, match := range matches {
		match = strings.TrimSpace(match)
		if match == "" {
			continue
		}
		key := strings.ToLower(match)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		parts = append(parts, match)
		if len(seen) >= 32 {
			break
		}
	}
	query := strings.Join(parts, " ")
	if len(query) > 2048 {
		query = query[:2048]
	}
	return strings.TrimSpace(query)
}

func boundedWords(input string, max int) string {
	words := strings.FieldsFunc(input, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsPunct(r) })
	if len(words) > max {
		words = words[:max]
	}
	return strings.Join(words, " ")
}

// BuildEvidencePacket creates a stable, deduplicated packet suitable for the
// initial Agent context. Results are already ranked by the retriever.
func BuildEvidencePacket(results []SearchResult, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 32 * 1024
	}
	seen := make(map[string]struct{})
	var builder strings.Builder
	for _, result := range results {
		key := result.Path + ":" + resultLineKey(result)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		entry := "- " + result.Path + ":" + resultLineKey(result) + " score=" + formatScore(result.Score) + "\n" + result.Snippet + "\n"
		if builder.Len()+len(entry) > maxBytes {
			break
		}
		builder.WriteString(entry)
	}
	return builder.String()
}

func resultLineKey(result SearchResult) string {
	return strings.Join([]string{itoa(result.StartLine), itoa(result.EndLine)}, "-")
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	return string(digits)
}

func formatScore(value float64) string {
	return strings.TrimRight(strings.TrimRight(fmtFloat(value), "0"), ".")
}

func fmtFloat(value float64) string {
	// Keep this helper local to avoid formatting differences in evidence hashes.
	return strconv.FormatFloat(value, 'f', 4, 64)
}

// Keep the packet helper deterministic even if callers sort an input slice in
// place after this function returns.
func SortResults(results []SearchResult) {
	sort.SliceStable(results, func(i, j int) bool { return results[i].Score > results[j].Score })
}
