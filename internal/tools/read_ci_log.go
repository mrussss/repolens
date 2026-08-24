package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"repolens/internal/llm"
)

type ReadCILogArgs struct {
	Keyword   string `json:"keyword,omitempty"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

type ReadCILogTool struct {
	ciLog string
}

func NewReadCILogTool(ciLog string) *ReadCILogTool {
	return &ReadCILogTool{ciLog: ciLog}
}

func (t *ReadCILogTool) Name() string {
	return "read_ci_log"
}

func (t *ReadCILogTool) Description() string {
	return "Read and inspect the submitted CI log or error log with line range and keyword filtering."
}

func (t *ReadCILogTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Type: "function",
		Function: struct {
			Name        string                 `json:"name"`
			Description string                 `json:"description"`
			Parameters  map[string]interface{} `json:"parameters"`
		}{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"keyword": map[string]interface{}{
						"type":        "string",
						"description": "Optional keyword or error code to filter log lines",
					},
					"start_line": map[string]interface{}{
						"type":        "integer",
						"description": "1-based starting line number (default: 1)",
					},
					"end_line": map[string]interface{}{
						"type":        "integer",
						"description": "1-based ending line number (default: 100)",
					},
				},
			},
		},
	}
}

func (t *ReadCILogTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	if t.ciLog == "" {
		return "No CI/error log provided with this diagnosis run.", nil
	}

	var args ReadCILogArgs
	if argsJSON != "" {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}

	lines := strings.Split(t.ciLog, "\n")
	totalLines := len(lines)

	if args.Keyword != "" {
		keywordLower := strings.ToLower(args.Keyword)
		var matched []string
		for i, l := range lines {
			if strings.Contains(strings.ToLower(l), keywordLower) {
				// Include surrounding context (-2 to +2 lines)
				start := i - 2
				if start < 0 {
					start = 0
				}
				end := i + 3
				if end > totalLines {
					end = totalLines
				}
				matched = append(matched, fmt.Sprintf("--- Match near line %d ---", i+1))
				for j := start; j < end; j++ {
					prefix := "  "
					if j == i {
						prefix = "> "
					}
					matched = append(matched, fmt.Sprintf("%s%d: %s", prefix, j+1, lines[j]))
				}
			}
		}
		if len(matched) == 0 {
			return fmt.Sprintf("No log lines found containing keyword '%s'", args.Keyword), nil
		}
		return strings.Join(matched, "\n"), nil
	}

	start := args.StartLine
	if start <= 0 {
		start = 1
	}
	end := args.EndLine
	if end <= 0 || end > totalLines {
		end = 100
		if end > totalLines {
			end = totalLines
		}
	}
	if start > end {
		return "", fmt.Errorf("invalid line range: %d to %d", start, end)
	}

	selected := lines[start-1 : end]
	var out []string
	for i, l := range selected {
		out = append(out, fmt.Sprintf("%d: %s", start+i, l))
	}

	return strings.Join(out, "\n"), nil
}
