package agent

import (
	"context"
	"fmt"

	"repolens/internal/diagnosis"
	"repolens/internal/llm"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
	"repolens/internal/sse"
	"repolens/internal/tools"
	"repolens/internal/trace"
	"repolens/internal/worker"
)

type AgentRuntimeExecutor struct {
	provider   llm.Provider
	retriever  retrieval.Retriever
	storeFS    snapshotstore.SnapshotStore
	traceStore trace.Store
	sseHub     *sse.Hub
	guardCfg   GuardConfig
}

func NewAgentRuntimeExecutor(
	provider llm.Provider,
	retriever retrieval.Retriever,
	storeFS snapshotstore.SnapshotStore,
	traceStore trace.Store,
	sseHub *sse.Hub,
	guardCfg GuardConfig,
) *AgentRuntimeExecutor {
	return &AgentRuntimeExecutor{
		provider:   provider,
		retriever:  retriever,
		storeFS:    storeFS,
		traceStore: traceStore,
		sseHub:     sseHub,
		guardCfg:   guardCfg,
	}
}

func (e *AgentRuntimeExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	registry := NewToolRegistry()

	// Register 4 Read-Only Tools for this diagnosis session
	searchTool := tools.NewSearchCodeTool(e.retriever, run.SnapshotID)
	readFileTool := tools.NewReadFileTool(e.storeFS, run.RepositoryID, run.SnapshotID)
	readDocsTool := tools.NewReadDocsTool(e.storeFS, run.RepositoryID, run.SnapshotID)
	readCILogTool := tools.NewReadCILogTool(run.ErrorLog)

	registry.Register(searchTool)
	registry.Register(readFileTool)
	registry.Register(readDocsTool)
	registry.Register(readCILogTool)

	loop := NewAgentLoop(e.provider, registry, e.traceStore, e.sseHub, e.guardCfg)
	res, err := loop.Run(ctx, run, attempt)
	if err != nil {
		return nil, fmt.Errorf("agent loop execution failed: %w", err)
	}

	return &worker.ExecutionResult{
		Report:           res.Report,
		RawOutput:        res.RawOutput,
		PromptTokens:     res.PromptTokens,
		CompletionTokens: res.CompletionTokens,
		ToolCalls:        res.ToolCallsCount,
		Retryable:        false,
	}, nil
}
