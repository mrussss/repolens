package retrieval

import (
	"context"
	"errors"
	"fmt"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval/bm25"
)

const maxBM25SymbolSourceBytes = 64 * 1024

// SymbolIndexSource pins source reads to the snapshot analyzed by CodeIntel.
// A nil source preserves metadata-only indexing for existing callers.
type SymbolIndexSource struct {
	Store        snapshotstore.SnapshotStore
	RepositoryID string
	SnapshotID   string
}

// BuildSymbolIndex is the production Symbol-to-document projection shared by
// the retrieval worker and RealBench. Callers supply persisted symbols in order.
func BuildSymbolIndex(ctx context.Context, symbols []*codeintelmodel.Symbol, source *SymbolIndexSource) (*bm25.Index, error) {
	var sourceBodies []string
	if source != nil {
		if source.Store == nil {
			return nil, fmt.Errorf("symbol index source store is required")
		}
		var err error
		sourceBodies, err = readSymbolSourceBodies(ctx, source.Store, source.RepositoryID, source.SnapshotID, symbols)
		if err != nil {
			return nil, fmt.Errorf("failed reading pinned snapshot source bodies: %w", err)
		}
	}
	idx := bm25.NewIndex(1.2, 0.75)
	for symbolIndex, sym := range symbols {
		content := fmt.Sprintf("%s %s %s %s %s", sym.Name, sym.QualifiedName, sym.ReceiverCanonical, sym.Signature, sym.Doc)
		if source != nil {
			content += "\n" + sourceBodies[symbolIndex]
		}
		idx.AddDocument(bm25.Document{
			FilePath:      sym.FilePath,
			StartLine:     sym.StartLine,
			EndLine:       sym.EndLine,
			Content:       content,
			SymbolKeyHash: sym.SymbolKeyHash,
			SymbolName:    sym.Name,
			Kind:          string(sym.Kind),
		})
	}
	idx.Build()

	return idx, nil
}

func readSymbolSourceBodies(ctx context.Context, storeFS snapshotstore.SnapshotStore, repoID, snapshotID string, symbols []*codeintelmodel.Symbol) ([]string, error) {
	bodies := make([]string, len(symbols))
	byPath := make(map[string][]int)
	for i, sym := range symbols {
		if sym.StartLine < 1 || sym.EndLine < sym.StartLine {
			return nil, fmt.Errorf("symbol %s has invalid source range %d-%d", sym.Name, sym.StartLine, sym.EndLine)
		}
		byPath[sym.FilePath] = append(byPath[sym.FilePath], i)
	}

	if batchReader, ok := storeFS.(interface {
		ReadFileRangesBounded(context.Context, string, string, string, []snapshotstore.LineRange, int) ([]snapshotstore.BoundedLineRangeResult, error)
	}); ok {
		for path, indexes := range byPath {
			ranges := make([]snapshotstore.LineRange, len(indexes))
			for i, symbolIndex := range indexes {
				sym := symbols[symbolIndex]
				ranges[i] = snapshotstore.LineRange{StartLine: sym.StartLine, EndLine: sym.EndLine}
			}
			results, err := batchReader.ReadFileRangesBounded(ctx, repoID, snapshotID, path, ranges, maxBM25SymbolSourceBytes)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", path, err)
			}
			if len(results) != len(indexes) {
				return nil, fmt.Errorf("read %s returned %d ranges, want %d", path, len(results), len(indexes))
			}
			for i, result := range results {
				if result.Err != nil && !errors.Is(result.Err, snapshotstore.ErrLineTooLong) {
					return nil, fmt.Errorf("symbol %s in %s: %w", symbols[indexes[i]].Name, path, result.Err)
				}
				if result.Err == nil {
					bodies[indexes[i]] = result.Content
				}
			}
		}
		return bodies, nil
	}

	for i, sym := range symbols {
		body, err := readSymbolSourceBody(ctx, storeFS, repoID, snapshotID, sym)
		if err != nil {
			return nil, fmt.Errorf("symbol %s in %s: %w", sym.Name, sym.FilePath, err)
		}
		bodies[i] = body
	}
	return bodies, nil
}

func readSymbolSourceBody(ctx context.Context, storeFS snapshotstore.SnapshotStore, repoID, snapshotID string, sym *codeintelmodel.Symbol) (string, error) {
	if sym.StartLine < 1 || sym.EndLine < sym.StartLine {
		return "", fmt.Errorf("invalid source range %d-%d", sym.StartLine, sym.EndLine)
	}
	var content string
	var err error
	if bounded, ok := storeFS.(interface {
		ReadFileRangeBounded(context.Context, string, string, string, int, int, int) (snapshotstore.FileRange, error)
	}); ok {
		var file snapshotstore.FileRange
		file, err = bounded.ReadFileRangeBounded(ctx, repoID, snapshotID, sym.FilePath, sym.StartLine, sym.EndLine, maxBM25SymbolSourceBytes)
		content = file.Content
	} else {
		var file snapshotstore.FileRange
		file, err = storeFS.ReadFileRange(ctx, repoID, snapshotID, sym.FilePath, sym.StartLine, sym.EndLine, maxBM25SymbolSourceBytes)
		content = file.Content
	}
	if errors.Is(err, snapshotstore.ErrLineTooLong) {
		// One pathological line should not fail the whole build. Metadata and
		// documentation still form a searchable document for this symbol.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return content, nil
}
