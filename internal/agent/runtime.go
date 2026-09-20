package agent

import (
	"context"
	"fmt"
	"strings"

	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
	"repolens/internal/tools"
	"repolens/internal/trace"
)

type ExecutionResult struct {
	Report             *evidence.DiagnosisReportData
	RawOutput          string
	PromptTokens       int
	CompletionTokens   int
	CachedPromptTokens int
	ReasoningTokens    int
	ToolCalls          int
	ToolNames          []string
	AgentRounds        int
	SearchCalls        int
	ProviderCalls      int
	StructuredReport   bool
	ParseError         string
	FinalizationReason string
	Retryable          bool
	ErrorCode          string
	ErrorMessage       string
}

// ExecutionError carries progress already made by an Agent attempt. The job
// layer uses it to stop an expensive full-flow retry after tools or a provider
// response have already been observed.
type ExecutionError struct {
	Err      error
	Progress *ExecutionResult
}

func (e *ExecutionError) Error() string { return e.Err.Error() }
func (e *ExecutionError) Unwrap() error { return e.Err }
func (e *ExecutionError) Progressed() bool {
	return e.Progress != nil && (e.Progress.ToolCalls > 0 || e.Progress.PromptTokens > 0 || e.Progress.CompletionTokens > 0)
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
	evidenceBytes   int
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

func (e *AgentRuntimeExecutor) WithEvidencePacketLimit(maxBytes int) *AgentRuntimeExecutor {
	e.evidenceBytes = maxBytes
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

	guardCfg := e.guardCfg
	if run.MaxAgentRounds > 0 {
		guardCfg.MaxSteps = run.MaxAgentRounds
	}
	if run.MaxToolCalls > 0 {
		guardCfg.MaxToolCalls = run.MaxToolCalls
	}
	if run.MaxSearchCalls > 0 {
		guardCfg.MaxSearchCalls = run.MaxSearchCalls
	}
	if run.MaxRepeatCalls > 0 {
		guardCfg.MaxRepeatCalls = run.MaxRepeatCalls
	}
	if run.MaxToolResultBytes > 0 {
		guardCfg.MaxToolResultBytes = run.MaxToolResultBytes
	}
	if run.MaxOutputTokens > 0 {
		guardCfg.MaxOutputTokens = run.MaxOutputTokens
	}
	packetBytes := e.evidenceBytes
	if run.MaxEvidencePacketBytes > 0 {
		packetBytes = run.MaxEvidencePacketBytes
	}
	loop := NewAgentLoop(provider, registry, e.traceStore, guardCfg)
	if e.retriever != nil {
		query := retrieval.BuildQuery(run.IssueTitle, run.IssueDescription, run.ErrorLog)
		results, searchErr := e.retriever.Search(ctx, retrieval.SearchRequest{
			SnapshotID: run.SnapshotID, CodeIndexBuildID: run.CodeIndexBuildID,
			RetrievalBuildID: run.RetrievalBuildID, Query: query, TopK: 8,
		})
		if searchErr != nil {
			return nil, jobs.NewPermanentError("RETRIEVAL_FAILED", "initial retrieval failed", searchErr)
		}
		for i := range results {
			if e.storeFS == nil || results[i].Path == "" {
				continue
			}
			startLine := results[i].StartLine
			endLine := results[i].EndLine
			if startLine <= 0 {
				startLine = 1
			}
			if endLine < startLine || endLine-startLine >= 80 {
				endLine = startLine + 79
			}
			if excerpt, readErr := e.storeFS.ReadFile(ctx, run.RepositoryID, run.SnapshotID, results[i].Path, startLine, endLine); readErr == nil && strings.TrimSpace(excerpt) != "" {
				results[i].Snippet = excerpt
			}
		}
		loop.WithInitialEvidence(query, retrieval.BuildEvidencePacket(results, packetBytes))
	}
	res, err := loop.Run(ctx, run, attempt)
	if err != nil {
		if res != nil {
			progress := &ExecutionResult{
				Report: res.Report, RawOutput: res.RawOutput,
				PromptTokens: res.PromptTokens, CompletionTokens: res.CompletionTokens,
				CachedPromptTokens: res.CachedPromptTokens, ReasoningTokens: res.ReasoningTokens,
				ToolCalls: res.ToolCallsCount, ToolNames: res.ToolNames, AgentRounds: res.AgentRounds,
				SearchCalls: res.SearchCalls, ProviderCalls: res.ProviderCalls,
				StructuredReport: res.StructuredReport, ParseError: res.ParseError,
				FinalizationReason: res.FinalizationReason,
			}
			return progress, &ExecutionError{Err: fmt.Errorf("agent loop execution failed: %w", err), Progress: progress}
		}
		return nil, fmt.Errorf("agent loop execution failed: %w", err)
	}

	return &ExecutionResult{
		Report:             res.Report,
		RawOutput:          res.RawOutput,
		PromptTokens:       res.PromptTokens,
		CompletionTokens:   res.CompletionTokens,
		CachedPromptTokens: res.CachedPromptTokens,
		ReasoningTokens:    res.ReasoningTokens,
		ToolCalls:          res.ToolCallsCount,
		ToolNames:          res.ToolNames,
		AgentRounds:        res.AgentRounds,
		SearchCalls:        res.SearchCalls,
		ProviderCalls:      res.ProviderCalls,
		StructuredReport:   res.StructuredReport,
		ParseError:         res.ParseError,
		FinalizationReason: res.FinalizationReason,
		Retryable:          false,
	}, nil
}
