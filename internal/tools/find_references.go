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
	return "Finds callers and references by exact symbol identity. If symbol_name is ambiguous, choose a candidate and retry with symbol_id, qualified_name, or symbol_key_hash."
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
					"symbol_key_hash": map[string]interface{}{
						"type":        "string",
						"description": "Exact deterministic Symbol identity hash",
					},
					"qualified_name": map[string]interface{}{
						"type":        "string",
						"description": "Exact qualified Symbol name",
					},
					"symbol_name": map[string]interface{}{
						"type":        "string",
						"description": "Exact unqualified Symbol name; ambiguous names return candidates",
					},
					"symbol_id": map[string]interface{}{
						"type":        "integer",
						"minimum":     1,
						"description": "Exact Symbol primary key ID",
					},
				},
			},
		},
	}
}

type findReferencesArgs struct {
	SymbolKeyHash string `json:"symbol_key_hash"`
	QualifiedName string `json:"qualified_name"`
	SymbolName    string `json:"symbol_name"`
	SymbolID      *int64 `json:"symbol_id"`
}

func (t *FindReferencesTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args findReferencesArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for find_references: %w", err)
	}

	if t.ciStore == nil || t.buildID <= 0 {
		return "Code intelligence relations graph is not available for this run.", nil
	}

	symbol, failure, err := resolveSymbol(ctx, t.ciStore, t.buildID, args.SymbolKeyHash, args.SymbolID, args.QualifiedName, args.SymbolName)
	if err != nil {
		return "", fmt.Errorf("failed resolving symbol identity: %w", err)
	}
	if failure != nil {
		return symbolResolutionFailureJSON(failure), nil
	}

	rels, err := t.ciStore.ListRelationsForSymbol(ctx, t.buildID, symbol.ID)
	if err != nil {
		return "", fmt.Errorf("failed fetching references: %w", err)
	}

	if len(rels) == 0 {
		return fmt.Sprintf("No references or callers found for symbol ID %d.", symbol.ID), nil
	}

	outBytes, err := json.MarshalIndent(rels, "", "  ")
	if err != nil {
		return "", err
	}

	return string(outBytes), nil
}
