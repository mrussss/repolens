package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"repolens/internal/llm"
	"repolens/internal/platform/metrics"
	"repolens/internal/retrieval"
)

type SearchCodeArgs struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k,omitempty"`
	Scope string `json:"scope,omitempty"`
}

type SearchCodeTool struct {
	retriever  retrieval.Retriever
	snapshotID string
}

func NewSearchCodeTool(retriever retrieval.Retriever, snapshotID string) *SearchCodeTool {
	return &SearchCodeTool{
		retriever:  retriever,
		snapshotID: snapshotID,
	}
}

func (t *SearchCodeTool) Name() string {
	return "search_code"
}

func (t *SearchCodeTool) Description() string {
	return "Search code snippets and symbols in the current repository snapshot using keywords or semantics."
}

func (t *SearchCodeTool) Definition() llm.ToolDefinition {
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
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Keyword, symbol name, error message, or function name to search for",
					},
					"top_k": map[string]interface{}{
						"type":        "integer",
						"description": "Number of top results to return (default: 5, max: 20)",
					},
					"scope": map[string]interface{}{
						"type":        "string",
						"description": "Optional file path prefix or directory to scope the search",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

func (t *SearchCodeTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args SearchCodeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	if args.Query == "" {
		return "", fmt.Errorf("query parameter cannot be empty")
	}
	if args.TopK <= 0 {
		args.TopK = 5
	}
	if args.TopK > 20 {
		args.TopK = 20
	}

	start := time.Now()
	results, err := t.retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: t.snapshotID,
		Query:      args.Query,
		TopK:       args.TopK,
		Scope:      args.Scope,
	})
	latency := time.Since(start).Seconds()
	metrics.RetrievalRequestsTotal.WithLabelValues("search_code").Inc()
	metrics.RetrievalLatencySeconds.WithLabelValues("search_code").Observe(latency)

	if err != nil {
		return "", fmt.Errorf("retrieval failed: %w", err)
	}

	if len(results) == 0 {
		return "No relevant code found matching query: " + args.Query, nil
	}

	outBytes, err := json.Marshal(results)
	if err != nil {
		return "", fmt.Errorf("failed to format search results: %w", err)
	}

	return string(outBytes), nil
}
