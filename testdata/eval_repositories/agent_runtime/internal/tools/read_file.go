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

var (
	blockedFilenames = map[string]bool{
		".env":                 true,
		".env.local":           true,
		".env.production":      true,
		"id_rsa":               true,
		"id_dsa":               true,
		"id_ed25519":           true,
		"credentials.json":     true,
		"service-account.json": true,
		".git/config":          true,
		".git/HEAD":            true,
	}

	blockedExtensions = map[string]bool{
		".pem": true,
		".key": true,
		".exe": true,
		".dll": true,
		".bin": true,
		".so":  true,
		".zip": true,
		".tar": true,
		".gz":  true,
		".png": true,
		".jpg": true,
		".pdf": true,
	}
)

type ReadFileArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
}

type ReadFileTool struct {
	storeFS    snapshotstore.SnapshotStore
	repoID     string
	snapshotID string
}

func NewReadFileTool(storeFS snapshotstore.SnapshotStore, repoID, snapshotID string) *ReadFileTool {
	return &ReadFileTool{
		storeFS:    storeFS,
		repoID:     repoID,
		snapshotID: snapshotID,
	}
}

func (t *ReadFileTool) Name() string {
	return "read_file"
}

func (t *ReadFileTool) Description() string {
	return "Read source file content in the repository snapshot by line range with security guards."
}

func (t *ReadFileTool) Definition() llm.ToolDefinition {
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
						"description": "Relative file path within the repository",
					},
					"start_line": map[string]interface{}{
						"type":        "integer",
						"description": "1-based starting line number (default: 1)",
					},
					"end_line": map[string]interface{}{
						"type":        "integer",
						"description": "1-based ending line number (default: total lines)",
					},
				},
				"required": []string{"path"},
			},
		},
	}
}

func (t *ReadFileTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args ReadFileArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	cleanPath := filepath.Clean(args.Path)
	if strings.HasPrefix(cleanPath, "..") || filepath.IsAbs(cleanPath) {
		return "", fmt.Errorf("path traversal denied: %s", args.Path)
	}

	base := filepath.Base(cleanPath)
	ext := strings.ToLower(filepath.Ext(cleanPath))

	if blockedFilenames[strings.ToLower(base)] || blockedFilenames[filepath.ToSlash(cleanPath)] {
		return "", fmt.Errorf("access to sensitive file denied: %s", cleanPath)
	}
	if blockedExtensions[ext] {
		return "", fmt.Errorf("access to binary/secret file type denied: %s", cleanPath)
	}

	if !t.storeFS.FileExists(t.repoID, t.snapshotID, cleanPath) {
		return "", fmt.Errorf("file not found in snapshot: %s", cleanPath)
	}

	content, err := t.storeFS.ReadFile(ctx, t.repoID, t.snapshotID, cleanPath, args.StartLine, args.EndLine)
	if err != nil {
		return "", err
	}

	// Truncate output if exceeding 64KB
	maxBytes := 64 * 1024
	if len(content) > maxBytes {
		content = content[:maxBytes] + "\n\n...[OUTPUT TRUNCATED DUE TO SIZE LIMIT]..."
	}

	return content, nil
}
