package agent

import (
	"context"
	"fmt"

	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/llm"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
	"repolens/internal/tools"
	"repolens/internal/trace"
)

type ExecutionResult struct {
	Report           *evidence.DiagnosisReportData
	RawOutput        string
	PromptTokens     int
	CompletionTokens int
	ToolCalls        int
	Retryable        bool
	ErrorCode        string
	ErrorMessage     string
}

type Executor interface {
	Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*ExecutionResult, error)
}

type AgentRuntimeExecutor struct {
	provider        llm.Provider
	providerFactory ProviderFactory
	retriever       retrieval.Retriever
	ciStore         codeintelstore.Store
	storeFS         snapshotstore.SnapshotStore
	traceStore      trace.Store
	guardCfg        GuardConfig
}

type ProviderFactory interface {
	BuildForDiagnosis(ctx context.Context, run *diagnosis.DiagnosisRun) (llm.Provider, error)
}

func NewAgentRuntimeExecutor(
	provider llm.Provider,
	retriever retrieval.Retriever,
	storeFS snapshotstore.SnapshotStore,
	traceStore trace.Store,
	guardCfg GuardConfig,
) *AgentRuntimeExecutor {
	return &AgentRuntimeExecutor{
		provider:   provider,
		retriever:  retriever,
		storeFS:    storeFS,
		traceStore: traceStore,
		guardCfg:   guardCfg,
	}
}

func NewAgentRuntimeExecutorWithFactory(factory ProviderFactory, retriever retrieval.Retriever, storeFS snapshotstore.SnapshotStore, traceStore trace.Store, guardCfg GuardConfig) *AgentRuntimeExecutor {
	return &AgentRuntimeExecutor{providerFactory: factory, retriever: retriever, storeFS: storeFS, traceStore: traceStore, guardCfg: guardCfg}
}

func (e *AgentRuntimeExecutor) WithCodeIntelStore(ciStore codeintelstore.Store) *AgentRuntimeExecutor {
	e.ciStore = ciStore
	return e
}

func (e *AgentRuntimeExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*ExecutionResult, error) {
	registry := NewToolRegistry()
	provider := e.provider
	if e.providerFactory != nil {
		var err error
		provider, err = e.providerFactory.BuildForDiagnosis(ctx, run)
		if err != nil {
			return nil, err
		}
	}
	if provider == nil {
		return nil, fmt.Errorf("no provider configured")
	}

	buildID := run.CodeIndexBuildID
	if e.ciStore != nil && ((run.CodeIndexBuildID == 0) != (run.RetrievalBuildID == 0)) {
		return nil, fmt.Errorf("incomplete pinned build lineage")
	}

	// Register 5 Read-Only Tools (Section 32 of Master Spec)
	searchTool := tools.NewPinnedSearchCodeTool(e.retriever, run.SnapshotID, run.CodeIndexBuildID, run.RetrievalBuildID)
	if run.CodeIndexBuildID == 0 && run.RetrievalBuildID == 0 {
		searchTool = tools.NewSearchCodeTool(e.retriever, run.SnapshotID)
	}
	getSymbolTool := tools.NewGetSymbolTool(e.ciStore, buildID)
	findRefTool := tools.NewFindReferencesTool(e.ciStore, buildID)
	findTestTool := tools.NewFindRelatedTestsTool(e.ciStore, buildID)
	readFileTool := tools.NewReadFileTool(e.storeFS, run.RepositoryID, run.SnapshotID)

	registry.Register(searchTool)
	registry.Register(getSymbolTool)
	registry.Register(findRefTool)
	registry.Register(findTestTool)
	registry.Register(readFileTool)

	loop := NewAgentLoop(provider, registry, e.traceStore, e.guardCfg)
	res, err := loop.Run(ctx, run, attempt)
	if err != nil {
		return nil, fmt.Errorf("agent loop execution failed: %w", err)
	}

	return &ExecutionResult{
		Report:           res.Report,
		RawOutput:        res.RawOutput,
		PromptTokens:     res.PromptTokens,
		CompletionTokens: res.CompletionTokens,
		ToolCalls:        res.ToolCallsCount,
		Retryable:        false,
	}, nil
}
