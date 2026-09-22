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
	ReportDraft        *evidence.ReportDraft
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
	FinishReason       string
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
	evidenceIssuer  evidence.EvidenceIssuer
	generation      *GenerationOptions
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

func (e *AgentRuntimeExecutor) WithEvidenceIssuer(issuer evidence.EvidenceIssuer) *AgentRuntimeExecutor {
	e.evidenceIssuer = issuer
	return e
}

func (e *AgentRuntimeExecutor) WithGenerationOptions(options GenerationOptions) *AgentRuntimeExecutor {
	e.generation = &options
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
	if e.evidenceIssuer != nil {
		maxEvidenceBytes := evidenceContentBudget(guardCfg.MaxToolResultBytes)
		searchTool.WithEvidenceIssuer(e.storeFS, e.evidenceIssuer, attempt.ID, run.ID, maxEvidenceBytes)
		searchTool.WithEvidenceRepositoryID(run.RepositoryID)
		readFileTool.WithEvidenceIssuer(e.evidenceIssuer, attempt.ID, run.ID, run.CodeIndexBuildID, maxEvidenceBytes)
	}
	packetBytes := e.evidenceBytes
	if run.MaxEvidencePacketBytes > 0 {
		packetBytes = run.MaxEvidencePacketBytes
	}
	generation := GenerationOptions{
		ReasoningEffort: strings.TrimSpace(run.ReasoningEffort),
		ResponseFormat:  &llm.ResponseFormat{Type: "json_object"},
	}
	if e.generation != nil {
		// RealBench may override response_format, but reasoning_effort is
		// always read from the immutable DiagnosisRun configuration snapshot.
		generation.ResponseFormat = e.generation.ResponseFormat
	}
	loop := NewAgentLoop(provider, registry, e.traceStore, guardCfg).WithGenerationOptions(generation)
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
			if e.evidenceIssuer != nil {
				item, issueErr := e.evidenceIssuer.Issue(ctx, evidence.IssueRequest{
					AttemptID:        attempt.ID,
					DiagnosisRunID:   run.ID,
					RepositoryID:     run.RepositoryID,
					SnapshotID:       run.SnapshotID,
					CodeIndexBuildID: run.CodeIndexBuildID,
					SourceKind:       evidence.SourceInitialRetrieval,
					RetrievalChunkID: results[i].ChunkID,
					FilePath:         results[i].Path,
					StartLine:        startLine,
					EndLine:          endLine,
					MaxBytes:         packetBytes,
				})
				if issueErr != nil {
					return nil, jobs.NewPermanentError("EVIDENCE_ISSUE_FAILED", "initial evidence could not be issued", issueErr)
				}
				results[i].EvidenceID = item.ID
				results[i].Path = item.FilePath
				results[i].StartLine = item.StartLine
				results[i].EndLine = item.EndLine
				results[i].Snippet = item.DisplayExcerpt
			} else if excerpt, readErr := e.storeFS.ReadFile(ctx, run.RepositoryID, run.SnapshotID, results[i].Path, startLine, endLine); readErr == nil && strings.TrimSpace(excerpt) != "" {
				results[i].Snippet = excerpt
			}
		}
		loop.WithInitialEvidence(query, retrieval.BuildEvidencePacket(results, packetBytes))
	}
	res, err := loop.Run(ctx, run, attempt)
	if err != nil {
		if res != nil {
			progress := &ExecutionResult{
				Report: resolveDraft(ctx, e.evidenceIssuer, res.ReportDraft, run, attempt), ReportDraft: res.ReportDraft, RawOutput: res.RawOutput,
				PromptTokens: res.PromptTokens, CompletionTokens: res.CompletionTokens,
				CachedPromptTokens: res.CachedPromptTokens, ReasoningTokens: res.ReasoningTokens,
				ToolCalls: res.ToolCallsCount, ToolNames: res.ToolNames, AgentRounds: res.AgentRounds,
				SearchCalls: res.SearchCalls, ProviderCalls: res.ProviderCalls, FinishReason: res.FinishReason,
				StructuredReport: res.StructuredReport, ParseError: res.ParseError,
				FinalizationReason: res.FinalizationReason,
			}
			return progress, &ExecutionError{Err: fmt.Errorf("agent loop execution failed: %w", err), Progress: progress}
		}
		return nil, fmt.Errorf("agent loop execution failed: %w", err)
	}

	return &ExecutionResult{
		Report:             resolveDraft(ctx, e.evidenceIssuer, res.ReportDraft, run, attempt),
		ReportDraft:        res.ReportDraft,
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
		FinishReason:       res.FinishReason,
		StructuredReport:   res.StructuredReport,
		ParseError:         res.ParseError,
		FinalizationReason: res.FinalizationReason,
		Retryable:          false,
	}, nil
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

func resolveDraft(ctx context.Context, issuer evidence.EvidenceIssuer, draft *evidence.ReportDraft, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) *evidence.DiagnosisReportData {
	if draft == nil {
		return &evidence.DiagnosisReportData{}
	}
	report, _ := evidence.ResolveReportDraft(ctx, issuer, draft, evidence.DraftLineage{
		AttemptID:        attempt.ID,
		DiagnosisRunID:   run.ID,
		RepositoryID:     run.RepositoryID,
		SnapshotID:       run.SnapshotID,
		CodeIndexBuildID: run.CodeIndexBuildID,
	})
	if report == nil {
		return &evidence.DiagnosisReportData{}
	}
	return report
}
