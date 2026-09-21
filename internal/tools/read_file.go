package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"repolens/internal/evidence"
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
	evidence   evidence.EvidenceIssuer
	attemptID  string
	runID      string
	buildID    int64
	maxBytes   int
}

func (t *ReadFileTool) WithEvidenceIssuer(issuer evidence.EvidenceIssuer, attemptID, runID string, buildID int64, maxBytes int) *ReadFileTool {
	t.evidence = issuer
	t.attemptID = attemptID
	t.runID = runID
	t.buildID = buildID
	t.maxBytes = maxBytes
	return t
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
	return "Read source file content in the repository snapshot by line range with security guards. The returned evidence_id is the only supported way to cite this source range in the final report."
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

	if t.evidence != nil {
		maxBytes := t.maxBytes
		if maxBytes <= 0 {
			maxBytes = 24 * 1024
		}
		item, err := t.evidence.Issue(ctx, evidence.IssueRequest{
			AttemptID:        t.attemptID,
			DiagnosisRunID:   t.runID,
			RepositoryID:     t.repoID,
			SnapshotID:       t.snapshotID,
			CodeIndexBuildID: t.buildID,
			SourceKind:       evidence.SourceReadFile,
			FilePath:         cleanPath,
			StartLine:        args.StartLine,
			EndLine:          args.EndLine,
			MaxBytes:         maxBytes,
		})
		if err != nil {
			return "", err
		}
		return marshalEvidenceResponse(item)
	}

	// Keep the legacy no-issuer path bounded at complete lines as well. The
	// evidence-enabled path above is canonical; neither path should slice a
	// source string after reading it.
	contentRange, err := t.storeFS.ReadFileRange(ctx, t.repoID, t.snapshotID, cleanPath, args.StartLine, args.EndLine, 64*1024)
	if err != nil {
		return "", err
	}
	return contentRange.Content, nil
}

type evidenceToolResponse struct {
	EvidenceID       string `json:"evidence_id,omitempty"`
	Path             string `json:"path"`
	StartLine        int    `json:"start_line"`
	EndLine          int    `json:"end_line"`
	TotalLines       int    `json:"total_lines"`
	Content          string `json:"content"`
	ContentHash      string `json:"content_hash"`
	RedactionApplied bool   `json:"redaction_applied"`
	Truncated        bool   `json:"truncated"`
}

func marshalEvidenceResponse(item *evidence.AttemptEvidenceItem) (string, error) {
	response, err := json.Marshal(evidenceToolResponse{
		EvidenceID:       item.ID,
		Path:             item.FilePath,
		StartLine:        item.StartLine,
		EndLine:          item.EndLine,
		TotalLines:       item.TotalLines,
		Content:          item.DisplayExcerpt,
		ContentHash:      item.RawContentHash,
		RedactionApplied: item.RedactionApplied,
		Truncated:        item.Truncated,
	})
	if err != nil {
		return "", fmt.Errorf("marshal evidence response: %w", err)
	}
	return string(response), nil
}
