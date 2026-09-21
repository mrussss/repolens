package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"repolens/internal/evidence"
	"repolens/internal/llm"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
)

type SearchCodeTool struct {
	retriever        retrieval.Retriever
	snapshotID       string
	codeIndexBuildID int64
	retrievalBuildID int64
	repositoryID     string
	storeFS          snapshotstore.SnapshotStore
	evidence         evidence.EvidenceIssuer
	attemptID        string
	runID            string
	maxBytes         int
}

func (t *SearchCodeTool) WithEvidenceIssuer(storeFS snapshotstore.SnapshotStore, issuer evidence.EvidenceIssuer, attemptID, runID string, maxBytes int) *SearchCodeTool {
	t.storeFS = storeFS
	t.evidence = issuer
	t.attemptID = attemptID
	t.runID = runID
	t.maxBytes = maxBytes
	return t
}

func (t *SearchCodeTool) WithEvidenceRepositoryID(repositoryID string) *SearchCodeTool {
	t.repositoryID = repositoryID
	return t
}

func NewPinnedSearchCodeTool(retriever retrieval.Retriever, snapshotID string, codeIndexBuildID, retrievalBuildID int64) *SearchCodeTool {
	return &SearchCodeTool{retriever: retriever, snapshotID: snapshotID, codeIndexBuildID: codeIndexBuildID, retrievalBuildID: retrievalBuildID}
}

func NewSearchCodeTool(retriever retrieval.Retriever, snapshotID string) *SearchCodeTool {
	return &SearchCodeTool{
		retriever:  retriever,
		snapshotID: snapshotID,
	}
}

func (t *SearchCodeTool) Name() string {
	return "search_code"
}

func (t *SearchCodeTool) Description() string {
	return "Searches the codebase using code-aware BM25 and structural code intelligence. Results include server-issued evidence_id values; only those IDs may be cited in the final report."
}

func (t *SearchCodeTool) Definition() llm.ToolDefinition {
	return llm.ToolDefinition{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        t.Name(),
			Description: t.Description(),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Code search query, function name, struct, or error message",
					},
					"top_k": map[string]interface{}{
						"type":        "integer",
						"description": "Maximum number of results to return (default 5)",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

type searchCodeArgs struct {
	Query string `json:"query"`
	TopK  int    `json:"top_k"`
}

func (t *SearchCodeTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args searchCodeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for search_code: %w", err)
	}

	topK := args.TopK
	if topK <= 0 {
		topK = 5
	}

	results, err := t.retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: t.snapshotID, CodeIndexBuildID: t.codeIndexBuildID,
		RetrievalBuildID: t.retrievalBuildID, Query: args.Query, TopK: topK,
	})
	if err != nil {
		return "", fmt.Errorf("retrieval failed: %w", err)
	}

	if len(results) == 0 {
		return "No code matches found for the query.", nil
	}
	if t.evidence != nil {
		maxBytes := t.maxBytes
		if maxBytes <= 0 {
			maxBytes = 24 * 1024
		}
		itemMaxBytes := maxBytes
		if topK > 1 {
			itemMaxBytes = maxBytes / topK
			if itemMaxBytes <= 0 {
				itemMaxBytes = 1
			}
		}
		for i := range results {
			if results[i].Path == "" {
				continue
			}
			item, issueErr := t.evidence.Issue(ctx, evidence.IssueRequest{
				AttemptID:        t.attemptID,
				DiagnosisRunID:   t.runID,
				RepositoryID:     t.repositoryID,
				SnapshotID:       t.snapshotID,
				CodeIndexBuildID: t.codeIndexBuildID,
				SourceKind:       evidence.SourceSearchCode,
				RetrievalChunkID: results[i].ChunkID,
				FilePath:         results[i].Path,
				StartLine:        results[i].StartLine,
				EndLine:          results[i].EndLine,
				MaxBytes:         itemMaxBytes,
			})
			if issueErr != nil {
				return "", issueErr
			}
			results[i].EvidenceID = item.ID
			results[i].StartLine = item.StartLine
			results[i].EndLine = item.EndLine
			results[i].Snippet = item.DisplayExcerpt
		}
		results = resultsWithinBudget(results, maxBytes)
	}

	outBytes, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", err
	}

	return string(outBytes), nil
}

func resultsWithinBudget(results []retrieval.SearchResult, maxBytes int) []retrieval.SearchResult {
	if maxBytes <= 0 {
		return results
	}
	accepted := make([]retrieval.SearchResult, 0, len(results))
	for _, result := range results {
		candidate := append(append([]retrieval.SearchResult(nil), accepted...), result)
		encoded, err := json.MarshalIndent(candidate, "", "  ")
		if err != nil || len(encoded) > maxBytes {
			break
		}
		accepted = append(accepted, result)
	}
	return accepted
}
