package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
						"minimum":     1,
						"maximum":     20,
						"description": "Maximum number of results to return (default 5, maximum 20)",
					},
				},
				"required": []string{"query"},
			},
		},
	}
}

type searchCodeArgs struct {
	Query string `json:"query"`
	TopK  *int   `json:"top_k"`
}

func (t *SearchCodeTool) Execute(ctx context.Context, argsJSON string) (string, error) {
	var args searchCodeArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("invalid arguments for search_code: %w", err)
	}

	topK := 5
	if args.TopK != nil {
		topK = *args.TopK
	}
	if topK < 1 || topK > 20 {
		return "", fmt.Errorf("top_k must be between 1 and 20")
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
	if len(results) > topK {
		results = results[:topK]
	}
	if t.evidence != nil {
		maxBytes := t.maxBytes
		if maxBytes <= 0 {
			maxBytes = 24 * 1024
		}
		eligible := results[:0]
		for _, result := range results {
			if result.Path != "" {
				eligible = append(eligible, result)
			}
		}
		results = eligible
		if len(results) == 0 {
			return "No code matches found for the query.", nil
		}
		itemMaxBytes := 0
		results, itemMaxBytes = searchResultsWithinEvidenceBudget(results, maxBytes)
		if len(results) == 0 {
			return "No code matches found for the query.", nil
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
				if i == 0 {
					return "", issueErr
				}
				results = results[:i]
				break
			}
			if item == nil {
				if i == 0 {
					return "", fmt.Errorf("evidence issuer returned an empty item")
				}
				results = results[:i]
				break
			}
			results[i].EvidenceID = item.ID
			results[i].StartLine = item.StartLine
			results[i].EndLine = item.EndLine
			results[i].Snippet = item.DisplayExcerpt
		}
	}

	outBytes, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", err
	}

	return string(outBytes), nil
}

func searchResultsWithinEvidenceBudget(results []retrieval.SearchResult, maxBytes int) ([]retrieval.SearchResult, int) {
	if maxBytes <= 0 {
		return results, maxBytes
	}
	accepted := make([]retrieval.SearchResult, 0, len(results))
	budgeted := make([]retrieval.SearchResult, 0, len(results))
	for _, result := range results {
		candidateResult := result
		// Reserve room for the opaque handle and a snippet field. The final
		// snippet allowance is calculated from the space left by these rows.
		candidateResult.EvidenceID = strings.Repeat("e", 64)
		candidateResult.Snippet = "x"
		candidate := append(append([]retrieval.SearchResult(nil), budgeted...), candidateResult)
		encoded, err := json.MarshalIndent(candidate, "", "  ")
		if err != nil || len(encoded) > maxBytes {
			break
		}
		accepted = append(accepted, result)
		budgeted = append(budgeted, candidateResult)
	}
	for len(accepted) > 0 {
		base, err := json.MarshalIndent(budgeted, "", "  ")
		if err != nil {
			return nil, 0
		}
		itemMaxBytes := (maxBytes - len(base)) / (len(budgeted) * 6)
		if itemMaxBytes < 1 {
			accepted = accepted[:len(accepted)-1]
			budgeted = budgeted[:len(budgeted)-1]
			continue
		}
		for index := range budgeted {
			budgeted[index].Snippet = strings.Repeat("x", itemMaxBytes*6)
		}
		encoded, err := json.MarshalIndent(budgeted, "", "  ")
		if err == nil && len(encoded) <= maxBytes {
			return accepted, itemMaxBytes
		}
		accepted = accepted[:len(accepted)-1]
		budgeted = budgeted[:len(budgeted)-1]
	}
	return nil, 0
}
