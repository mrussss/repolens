package baseline

import (
	"regexp"
	"strings"
)

// WindowResult represents a fixed-window text match from the V1 baseline.
type WindowResult struct {
	FilePath  string `json:"file_path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Content   string `json:"content"`
	Score     float64 `json:"score"`
}

// V1RegexWindowRetriever implements the V1 regex and text window search for eval comparison.
type V1RegexWindowRetriever struct {
	WindowSize int
}

// NewV1Baseline creates a new V1 regex/window baseline retriever.
func NewV1Baseline(windowSize int) *V1RegexWindowRetriever {
	if windowSize <= 0 {
		windowSize = 40
	}
	return &V1RegexWindowRetriever{WindowSize: windowSize}
}

// Search scans lines of text for query terms using case-insensitive regex matching.
func (b *V1Baseline) SearchFile(filePath, content, query string) []WindowResult {
	if content == "" || query == "" {
		return nil
	}

	pattern := regexp.QuoteMeta(strings.TrimSpace(query))
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return nil
	}

	lines := strings.Split(content, "\n")
	var results []WindowResult

	for i, line := range lines {
		if re.MatchString(line) {
			start := max(0, i-b.WindowSize/2)
			end := min(len(lines), i+b.WindowSize/2)
			excerpt := strings.Join(lines[start:end], "\n")

			results = append(results, WindowResult{
				FilePath:  filePath,
				StartLine: start + 1,
				EndLine:   end,
				Content:   excerpt,
				Score:     1.0,
			})
		}
	}

	return results
}

type V1Baseline = V1RegexWindowRetriever

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
