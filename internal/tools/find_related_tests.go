package tools

import (
	"context"
	"encoding/json"
	"fmt"

	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/llm"
)

type FindRelatedTestsTool struct {
	ciStore codeintelstore.Store
	buildID int64
}

func NewFindRelatedTestsTool(ciStore codeintelstore.Store, buildID int64) *FindRelatedTestsTool {
	return &FindRelatedTestsTool{
		ciStore: ciStore,
		buildID: buildID,
	}
}

func (t *FindRelatedTestsTool) Name() string {
	return "find_related_tests"
}

func (t *FindRelatedTestsTool) Description() string {
	return "Discovers test functions, test files, and test relations linked to a production symbol."
}

func (t *FindRelatedTestsTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"symbol_name": map[string]interface{}{
						"type":        "string",
						"description": "Production symbol name (e.g. ProcessOrder)",
					},
					"symbol_key_hash": map[string]interface{}{
						"type":        "string",
						"description": "Symbol key hash if known",
					},
				},
				"required": []string{"symbol_name"},
			},
		},
	}
}

type findRelatedTestsArgs struct {
	SymbolName    string `json:"symbol_name"`
	SymbolKeyHash string `json:"symbol_key_hash"`
}

func (t *FindRelatedTestsTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args findRelatedTestsArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for find_related_tests: %w", err)
	}

	if t.ciStore == nil || t.buildID <= 0 {
		return "Related tests index is not available for this run.", nil
	}

	keyHash := args.SymbolKeyHash
	if keyHash == "" {
		symbols, err := t.ciStore.ListSymbols(ctx, t.buildID, args.SymbolName, 1)
		if err != nil || len(symbols) == 0 {
			return fmt.Sprintf("Symbol %q not found to discover related tests.", args.SymbolName), nil
		}
		keyHash = symbols[0].SymbolKeyHash
	}

	tests, err := t.ciStore.ListRelatedTests(ctx, t.buildID, keyHash)
	if err != nil {
		return "", fmt.Errorf("failed fetching related tests: %w", err)
	}

	if len(tests) == 0 {
		return fmt.Sprintf("No related tests found for %q.", args.SymbolName), nil
	}

	outBytes, err := json.MarshalIndent(tests, "", "  ")
	if err != nil {
		return "", err
	}

	return string(outBytes), nil
}
