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
	return "Discovers tests linked to an exact Symbol identity. If symbol_name is ambiguous, choose a candidate and retry with symbol_id, qualified_name, or symbol_key_hash."
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
					"symbol_id": map[string]interface{}{
						"type":        "integer",
						"minimum":     1,
						"description": "Exact Symbol primary key ID",
					},
					"qualified_name": map[string]interface{}{
						"type":        "string",
						"description": "Exact qualified Symbol name",
					},
					"symbol_name": map[string]interface{}{
						"type":        "string",
						"description": "Exact unqualified Symbol name; ambiguous names return candidates",
					},
					"symbol_key_hash": map[string]interface{}{
						"type":        "string",
						"description": "Symbol key hash if known",
					},
				},
			},
		},
	}
}

type findRelatedTestsArgs struct {
	SymbolName    string `json:"symbol_name"`
	SymbolKeyHash string `json:"symbol_key_hash"`
	QualifiedName string `json:"qualified_name"`
	SymbolID      *int64 `json:"symbol_id"`
}

func (t *FindRelatedTestsTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args findRelatedTestsArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for find_related_tests: %w", err)
	}

	if t.ciStore == nil || t.buildID <= 0 {
		return "Related tests index is not available for this run.", nil
	}

	symbol, failure, err := resolveSymbol(ctx, t.ciStore, t.buildID, args.SymbolKeyHash, args.SymbolID, args.QualifiedName, args.SymbolName)
	if err != nil {
		return "", fmt.Errorf("failed resolving symbol identity: %w", err)
	}
	if failure != nil {
		return symbolResolutionFailureJSON(failure), nil
	}

	tests, err := t.ciStore.ListRelatedTests(ctx, t.buildID, symbol.SymbolKeyHash)
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
