package tools

import (
	"context"
	"encoding/json"
	"fmt"

	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/llm"
)

type FindReferencesTool struct {
	ciStore codeintelstore.Store
	buildID int64
}

func NewFindReferencesTool(ciStore codeintelstore.Store, buildID int64) *FindReferencesTool {
	return &FindReferencesTool{
		ciStore: ciStore,
		buildID: buildID,
	}
}

func (t *FindReferencesTool) Name() string {
	return "find_references"
}

func (t *FindReferencesTool) Description() string {
	return "Finds cross-package callers, candidate call expressions, and references to a symbol."
}

func (t *FindReferencesTool) Definition() llm.ToolDefinition {
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
						"description": "Symbol name to look up references for",
					},
					"symbol_id": map[string]interface{}{
						"type":        "integer",
						"description": "Symbol primary key ID if known",
					},
				},
				"required": []string{"symbol_name"},
			},
		},
	}
}

type findReferencesArgs struct {
	SymbolName string `json:"symbol_name"`
	SymbolID   int64  `json:"symbol_id"`
}

func (t *FindReferencesTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args findReferencesArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for find_references: %w", err)
	}

	if t.ciStore == nil || t.buildID <= 0 {
		return "Code intelligence relations graph is not available for this run.", nil
	}

	symID := args.SymbolID
	if symID <= 0 {
		symbols, err := t.ciStore.ListSymbols(ctx, t.buildID, args.SymbolName, 1)
		if err != nil || len(symbols) == 0 {
			return fmt.Sprintf("Symbol %q not found to look up references.", args.SymbolName), nil
		}
		symID = symbols[0].ID
	}

	rels, err := t.ciStore.ListRelationsForSymbol(ctx, t.buildID, symID)
	if err != nil {
		return "", fmt.Errorf("failed fetching references: %w", err)
	}

	if len(rels) == 0 {
		return fmt.Sprintf("No references or callers found for symbol ID %d.", symID), nil
	}

	outBytes, err := json.MarshalIndent(rels, "", "  ")
	if err != nil {
		return "", err
	}

	return string(outBytes), nil
}
