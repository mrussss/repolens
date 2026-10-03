package tools

import (
	"context"
	"encoding/json"
	"errors"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
)

type symbolResolutionFailure struct {
	Code       string                      `json:"code"`
	Message    string                      `json:"message"`
	Candidates []symbolResolutionCandidate `json:"-"`
}

type symbolResolutionCandidate struct {
	ID            int64                     `json:"id"`
	Name          string                    `json:"name"`
	QualifiedName string                    `json:"qualified_name"`
	Kind          codeintelmodel.SymbolKind `json:"kind"`
	PackagePath   string                    `json:"package_path,omitempty"`
	PackageName   string                    `json:"package_name,omitempty"`
	ModulePath    string                    `json:"module_path,omitempty"`
	SymbolKeyHash string                    `json:"symbol_key_hash"`
}

// resolveSymbol turns an Agent supplied identity into exactly one Symbol.
// Search APIs retain their fuzzy behavior; identity resolution does not.
func resolveSymbol(ctx context.Context, ciStore codeintelstore.Store, buildID int64, symbolKeyHash string, symbolID *int64, qualifiedName, symbolName string) (*codeintelmodel.Symbol, *symbolResolutionFailure, error) {
	if symbolKeyHash != "" {
		symbol, failure, err := resolveExactSymbolMatches(ctx, ciStore, buildID, "symbol_key_hash", symbolKeyHash)
		if failure != nil && failure.Code == "SYMBOL_NOT_FOUND" {
			failure.Message = "no symbol has the supplied symbol_key_hash in this CodeIndexBuild"
		}
		return symbol, failure, err
	}
	if symbolID != nil {
		if *symbolID <= 0 {
			return nil, &symbolResolutionFailure{Code: "SYMBOL_NOT_FOUND", Message: "symbol_id must identify a symbol in this CodeIndexBuild"}, nil
		}
		symbol, err := ciStore.GetSymbolByID(ctx, *symbolID)
		if errors.Is(err, codeintelstore.ErrSymbolNotFound) {
			return nil, &symbolResolutionFailure{Code: "SYMBOL_NOT_FOUND", Message: "no symbol exists with the supplied symbol_id"}, nil
		}
		if err != nil {
			return nil, nil, err
		}
		if symbol.CodeIndexBuildID != buildID {
			return nil, &symbolResolutionFailure{Code: "SYMBOL_BUILD_MISMATCH", Message: "symbol_id does not belong to this CodeIndexBuild"}, nil
		}
		return symbol, nil, nil
	}
	if qualifiedName != "" {
		return resolveExactSymbolMatches(ctx, ciStore, buildID, "qualified_name", qualifiedName)
	}
	if symbolName != "" {
		return resolveExactSymbolMatches(ctx, ciStore, buildID, "name", symbolName)
	}
	return nil, &symbolResolutionFailure{Code: "SYMBOL_NOT_FOUND", Message: "provide symbol_key_hash, symbol_id, qualified_name, or symbol_name"}, nil
}

func resolveExactSymbolMatches(ctx context.Context, ciStore codeintelstore.Store, buildID int64, field, value string) (*codeintelmodel.Symbol, *symbolResolutionFailure, error) {
	symbols, err := ciStore.ListAllSymbols(ctx, buildID)
	if err != nil {
		return nil, nil, err
	}
	matches := make([]*codeintelmodel.Symbol, 0, 2)
	for _, symbol := range symbols {
		matched := field == "qualified_name" && symbol.QualifiedName == value ||
			field == "name" && symbol.Name == value ||
			field == "symbol_key_hash" && symbol.SymbolKeyHash == value
		if matched {
			matches = append(matches, symbol)
		}
	}
	switch len(matches) {
	case 0:
		return nil, &symbolResolutionFailure{Code: "SYMBOL_NOT_FOUND", Message: "no symbol matches the supplied exact identity in this CodeIndexBuild"}, nil
	case 1:
		return matches[0], nil, nil
	default:
		candidates := make([]symbolResolutionCandidate, 0, len(matches))
		for _, symbol := range matches {
			candidates = append(candidates, symbolResolutionCandidate{
				ID: symbol.ID, Name: symbol.Name, QualifiedName: symbol.QualifiedName, Kind: symbol.Kind,
				PackagePath: symbol.PackagePath, PackageName: symbol.PackageName,
				ModulePath: symbol.ModulePath, SymbolKeyHash: symbol.SymbolKeyHash,
			})
		}
		return nil, &symbolResolutionFailure{
			Code: "AMBIGUOUS_SYMBOL", Message: "the supplied symbol identity matches multiple symbols; retry with symbol_key_hash, symbol_id, or qualified_name",
			Candidates: candidates,
		}, nil
	}
}

func symbolResolutionFailureJSON(failure *symbolResolutionFailure) string {
	encoded, _ := json.Marshal(struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Candidates []symbolResolutionCandidate `json:"candidates,omitempty"`
	}{
		Error: struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}{Code: failure.Code, Message: failure.Message},
		Candidates: failure.Candidates,
	})
	return string(encoded)
}
