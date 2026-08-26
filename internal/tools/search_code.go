package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"repolens/internal/llm"
	"repolens/internal/retrieval"
)

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
	return "Searches the codebase using code-aware BM25 and structural code intelligence. Returns ranked code symbols, files, scores, and structural reasoning."
}

func (t *SearchCodeTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Code search query, function name, struct, or error message",
					},
					"top_k": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of results to return (default 5)",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

type searchCodeArgs struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

func (t *SearchCodeTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args searchCodeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for search_code: %w", err)
	}

	topK := args.TopK
	if topK <= 0 {
		topK = 5
	}

	results, err := t.retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: t.snapshotID,
		Query:      args.Query,
		TopK:       topK,
	})
	if err != nil {
		return "", fmt.Errorf("retrieval failed: %w", err)
	}

	if len(results) == 0 {
		return "No code matches found for the query.", nil
	}

	outBytes, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", err
	}

	return string(outBytes), nil
}
