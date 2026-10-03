package tools

import (
	"context"
	"encoding/json"
	"fmt"

	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/llm"
)

type GetSymbolTool struct {
	ciStore codeintelstore.Store
	buildID int64
}

func NewGetSymbolTool(ciStore codeintelstore.Store, buildID int64) *GetSymbolTool {
	return &GetSymbolTool{
		ciStore: ciStore,
		buildID: buildID,
	}
}

func (t *GetSymbolTool) Name() string {
	return "get_symbol"
}

func (t *GetSymbolTool) Description() string {
	return "Retrieves one authoritative Symbol by exact identity. An ambiguous name returns candidates; retry with symbol_id, qualified_name, or symbol_key_hash."
}

func (t *GetSymbolTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"name": map[string]interface{}{
						"type":        "string",
						"description": "Exact unqualified Symbol name (legacy input name)",
					},
					"symbol_name": map[string]interface{}{
						"type":        "string",
						"description": "Exact unqualified Symbol name",
					},
					"symbol_key_hash": map[string]interface{}{
						"type":        "string",
						"description": "Exact deterministic SHA256 Symbol key hash",
					},
					"symbol_id": map[string]interface{}{
						"type":        "integer",
						"minimum":     1,
						"description": "Exact Symbol primary key ID",
					},
					"qualified_name": map[string]interface{}{
						"type":        "string",
						"description": "Exact qualified Symbol name",
					},
				},
			},
		},
	}
}

type getSymbolArgs struct {
	Name          string `json:"name"`
	SymbolName    string `json:"symbol_name"`
	SymbolKeyHash string `json:"symbol_key_hash"`
	SymbolID      *int64 `json:"symbol_id"`
	QualifiedName string `json:"qualified_name"`
}

func (t *GetSymbolTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args getSymbolArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for get_symbol: %w", err)
	}

	if t.ciStore == nil || t.buildID <= 0 {
		return "Code intelligence symbol index is not available for this run.", nil
	}

	name := args.Name
	if name == "" {
		name = args.SymbolName
	}
	symbol, failure, err := resolveSymbol(ctx, t.ciStore, t.buildID, args.SymbolKeyHash, args.SymbolID, args.QualifiedName, name)
	if err != nil {
		return "", fmt.Errorf("failed resolving symbol identity: %w", err)
	}
	if failure != nil {
		return symbolResolutionFailureJSON(failure), nil
	}

	outBytes, err := json.MarshalIndent(symbol, "", "  ")
	if err != nil {
		return "", err
	}

	return string(outBytes), nil
}
