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
	return "Retrieves authoritative symbol definitions, signature, canonical receiver, and AST location by symbol name or key hash."
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
						"description": "Symbol name (e.g. ProcessOrder, ValidateToken)",
					},
					"symbol_key_hash": map[string]interface{}{
						"type":        "string",
						"description": "Deterministic SHA256 symbol key hash (optional)",
					},
				},
				"required": []string{"name"},
			},
		},
	}
}

type getSymbolArgs struct {
	Name          string `json:"name"`
	SymbolKeyHash string `json:"symbol_key_hash"`
}

func (t *GetSymbolTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args getSymbolArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for get_symbol: %w", err)
	}

	if t.ciStore == nil || t.buildID <= 0 {
		return "Code intelligence symbol index is not available for this run.", nil
	}

	if args.SymbolKeyHash != "" {
		sym, err := t.ciStore.GetSymbolByHash(ctx, t.buildID, args.SymbolKeyHash)
		if err == nil && sym != nil {
			outBytes, _ := json.MarshalIndent(sym, "", "  ")
			return string(outBytes), nil
		}
	}

	symbols, err := t.ciStore.ListSymbols(ctx, t.buildID, args.Name, 5)
	if err != nil {
		return "", fmt.Errorf("failed fetching symbols: %w", err)
	}

	if len(symbols) == 0 {
		return fmt.Sprintf("No symbol matching %q found in the index.", args.Name), nil
	}

	outBytes, err := json.MarshalIndent(symbols, "", "  ")
	if err != nil {
		return "", err
	}

	return string(outBytes), nil
}
