package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"repolens/internal/llm"
	"repolens/internal/platform/snapshotstore"
)

type ReadDocsArgs struct {
	Path string `json:"path"`
}

type ReadDocsTool struct {
	storeFS    snapshotstore.SnapshotStore
	repoID     string
	snapshotID string
}

func NewReadDocsTool(storeFS snapshotstore.SnapshotStore, repoID, snapshotID string) *ReadDocsTool {
	return &ReadDocsTool{
		storeFS:    storeFS,
		repoID:     repoID,
		snapshotID: snapshotID,
	}
}

func (t *ReadDocsTool) Name() string {
	return "read_docs"
}

func (t *ReadDocsTool) Description() string {
	return "Read documentation files (README, docs/**, markdown, architecture specs) in the repository."
}

func (t *ReadDocsTool) Definition() llm.ToolDefinition {
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
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Path to documentation file (e.g. README.md, docs/architecture.md)",
					},
				},
				"required": []string{"path"},
			},
		},
	}
}

func (t *ReadDocsTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args ReadDocsArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	cleanPath := filepath.Clean(args.Path)
	if strings.HasPrefix(cleanPath, "..") || filepath.IsAbs(cleanPath) {
		return "", fmt.Errorf("path traversal denied: %s", args.Path)
	}

	ext := strings.ToLower(filepath.Ext(cleanPath))
	base := strings.ToLower(filepath.Base(cleanPath))

	isDoc := ext == ".md" || ext == ".txt" || ext == ".rst" || strings.HasPrefix(base, "readme") || strings.HasPrefix(cleanPath, "docs")
	if !isDoc {
		return "", fmt.Errorf("read_docs only supports documentation files (*.md, *.txt, *.rst, docs/**, README)")
	}

	if !t.storeFS.FileExists(t.repoID, t.snapshotID, cleanPath) {
		return "", fmt.Errorf("document not found: %s", cleanPath)
	}

	content, err := t.storeFS.ReadFile(ctx, t.repoID, t.snapshotID, cleanPath, 1, 500)
	if err != nil {
		return "", err
	}

	return content, nil
}
