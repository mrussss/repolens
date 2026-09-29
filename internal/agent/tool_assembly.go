package agent

import (
	"fmt"

	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
	"repolens/internal/tools"
)

type ToolDependencies struct {
	Retriever      retrieval.Retriever
	CodeIntelStore codeintelstore.Store
	SnapshotStore  snapshotstore.SnapshotStore
	EvidenceIssuer evidence.EvidenceIssuer
}

func BuildToolRegistry(deps ToolDependencies, spec diagnosis.DiagnosisExecutionSpec, attemptID string) (*ToolRegistry, error) {
	registry := NewToolRegistry()
	buildID := spec.Lineage.CodeIndexBuildID
	if deps.CodeIntelStore != nil && ((buildID == 0) != (spec.Lineage.RetrievalBuildID == 0)) {
		return nil, fmt.Errorf("incomplete pinned build lineage")
	}

	searchTool := tools.NewPinnedSearchCodeTool(deps.Retriever, spec.Lineage.SnapshotID, buildID, spec.Lineage.RetrievalBuildID)
	if buildID == 0 && spec.Lineage.RetrievalBuildID == 0 {
		searchTool = tools.NewSearchCodeTool(deps.Retriever, spec.Lineage.SnapshotID)
	}
	getSymbolTool := tools.NewGetSymbolTool(deps.CodeIntelStore, buildID)
	findRefTool := tools.NewFindReferencesTool(deps.CodeIntelStore, buildID)
	findTestTool := tools.NewFindRelatedTestsTool(deps.CodeIntelStore, buildID)
	readFileTool := tools.NewReadFileTool(deps.SnapshotStore, spec.Lineage.RepositoryID, spec.Lineage.SnapshotID)

	if deps.EvidenceIssuer != nil {
		maxEvidenceBytes := evidenceContentBudget(spec.Budget.MaxToolResultBytes)
		searchTool.WithEvidenceIssuer(deps.SnapshotStore, deps.EvidenceIssuer, attemptID, spec.RunID, maxEvidenceBytes)
		searchTool.WithEvidenceRepositoryID(spec.Lineage.RepositoryID)
		readFileTool.WithEvidenceIssuer(deps.EvidenceIssuer, attemptID, spec.RunID, buildID, maxEvidenceBytes)
	}

	registry.Register(searchTool)
	registry.Register(getSymbolTool)
	registry.Register(findRefTool)
	registry.Register(findTestTool)
	registry.Register(readFileTool)
	return registry, nil
}

func evidenceContentBudget(toolResultLimit int) int {
	if toolResultLimit <= 0 {
		toolResultLimit = 32 * 1024
	}
	// Leave room for the structured evidence envelope (and, for search_code,
	// the JSON array and metadata) so the Agent loop never needs to slice a
	// canonical response after the tool has returned it.
	const envelopeReserve = 4096
	if toolResultLimit > envelopeReserve {
		return toolResultLimit - envelopeReserve
	}
	return toolResultLimit
}
