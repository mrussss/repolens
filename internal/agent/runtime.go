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
	Execute(ctx context.Context, spec diagnosis.DiagnosisExecutionSpec, attempt *diagnosis.DiagnosisAttempt) (*ExecutionResult, error)
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
	BuildForExecution(ctx context.Context, provider diagnosis.ProviderSnapshot) (llm.Provider, error)
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

func (e *AgentRuntimeExecutor) Execute(ctx context.Context, spec diagnosis.DiagnosisExecutionSpec, attempt *diagnosis.DiagnosisAttempt) (*ExecutionResult, error) {
	registry := NewToolRegistry()
	provider := e.provider
	if e.providerFactory != nil {
		var err error
		provider, err = e.providerFactory.BuildForExecution(ctx, spec.Provider)
		if err != nil {
			return nil, err
		}
	}
	if provider == nil {
		return nil, fmt.Errorf("no provider configured")
	}

	buildID := spec.Lineage.CodeIndexBuildID
	if e.ciStore != nil && ((spec.Lineage.CodeIndexBuildID == 0) != (spec.Lineage.RetrievalBuildID == 0)) {
		return nil, fmt.Errorf("incomplete pinned build lineage")
	}

	// Register 5 Read-Only Tools (Section 32 of Master Spec)
	searchTool := tools.NewPinnedSearchCodeTool(e.retriever, spec.Lineage.SnapshotID, spec.Lineage.CodeIndexBuildID, spec.Lineage.RetrievalBuildID)
	if spec.Lineage.CodeIndexBuildID == 0 && spec.Lineage.RetrievalBuildID == 0 {
		searchTool = tools.NewSearchCodeTool(e.retriever, spec.Lineage.SnapshotID)
	}
	getSymbolTool := tools.NewGetSymbolTool(e.ciStore, buildID)
	findRefTool := tools.NewFindReferencesTool(e.ciStore, buildID)
	findTestTool := tools.NewFindRelatedTestsTool(e.ciStore, buildID)
	readFileTool := tools.NewReadFileTool(e.storeFS, spec.Lineage.RepositoryID, spec.Lineage.SnapshotID)

	registry.Register(searchTool)
	registry.Register(getSymbolTool)
	registry.Register(findRefTool)
	registry.Register(findTestTool)
	registry.Register(readFileTool)

	guardCfg := e.guardCfg
	if spec.Budget.MaxAgentRounds > 0 {
		guardCfg.MaxSteps = spec.Budget.MaxAgentRounds
	}
	if spec.Budget.MaxToolCalls > 0 {
		guardCfg.MaxToolCalls = spec.Budget.MaxToolCalls
	}
	if spec.Budget.MaxSearchCalls > 0 {
		guardCfg.MaxSearchCalls = spec.Budget.MaxSearchCalls
	}
	if spec.Budget.MaxRepeatCalls > 0 {
		guardCfg.MaxRepeatCalls = spec.Budget.MaxRepeatCalls
	}
	if spec.Budget.MaxToolResultBytes > 0 {
		guardCfg.MaxToolResultBytes = spec.Budget.MaxToolResultBytes
	}
	if spec.Generation.MaxOutputTokens > 0 {
		guardCfg.MaxOutputTokens = spec.Generation.MaxOutputTokens
	}
	if e.evidenceIssuer != nil {
		maxEvidenceBytes := evidenceContentBudget(guardCfg.MaxToolResultBytes)
		searchTool.WithEvidenceIssuer(e.storeFS, e.evidenceIssuer, attempt.ID, spec.RunID, maxEvidenceBytes)
		searchTool.WithEvidenceRepositoryID(spec.Lineage.RepositoryID)
		readFileTool.WithEvidenceIssuer(e.evidenceIssuer, attempt.ID, spec.RunID, spec.Lineage.CodeIndexBuildID, maxEvidenceBytes)
	}
	packetBytes := e.evidenceBytes
	if spec.Budget.MaxEvidencePacketBytes > 0 {
		packetBytes = spec.Budget.MaxEvidencePacketBytes
	}
	generation := GenerationOptions{
		ReasoningEffort: strings.TrimSpace(spec.Generation.ReasoningEffort),
		ResponseFormat:  &llm.ResponseFormat{Type: "json_object"},
	}
	if e.generation != nil {
		// RealBench may override response_format, but reasoning_effort is
		// always read from the immutable DiagnosisRun configuration snapshot.
		generation.ResponseFormat = e.generation.ResponseFormat
	}
	loop := NewAgentLoop(provider, registry, e.traceStore, guardCfg).WithGenerationOptions(generation)
	if e.retriever != nil {
		query := retrieval.BuildQuery(spec.Issue.Title, spec.Issue.Description, spec.Issue.ErrorLog)
		results, searchErr := e.retriever.Search(ctx, retrieval.SearchRequest{
			SnapshotID: spec.Lineage.SnapshotID, CodeIndexBuildID: spec.Lineage.CodeIndexBuildID,
			RetrievalBuildID: spec.Lineage.RetrievalBuildID, Query: query, TopK: 8,
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
					DiagnosisRunID:   spec.RunID,
					RepositoryID:     spec.Lineage.RepositoryID,
					SnapshotID:       spec.Lineage.SnapshotID,
					CodeIndexBuildID: spec.Lineage.CodeIndexBuildID,
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
			} else if excerpt, readErr := e.storeFS.ReadFile(ctx, spec.Lineage.RepositoryID, spec.Lineage.SnapshotID, results[i].Path, startLine, endLine); readErr == nil && strings.TrimSpace(excerpt) != "" {
				results[i].Snippet = excerpt
			}
		}
		loop.WithInitialEvidence(query, retrieval.BuildEvidencePacket(results, packetBytes))
	}
	res, err := loop.Run(ctx, spec, attempt)
	if err != nil {
		if res != nil {
			progress := &ExecutionResult{
				Report: resolveDraft(ctx, e.evidenceIssuer, res.ReportDraft, spec, attempt), ReportDraft: res.ReportDraft, RawOutput: res.RawOutput,
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
		Report:             resolveDraft(ctx, e.evidenceIssuer, res.ReportDraft, spec, attempt),
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

func resolveDraft(ctx context.Context, issuer evidence.EvidenceIssuer, draft *evidence.ReportDraft, spec diagnosis.DiagnosisExecutionSpec, attempt *diagnosis.DiagnosisAttempt) *evidence.DiagnosisReportData {
	if draft == nil {
		return &evidence.DiagnosisReportData{}
	}
	report, _ := evidence.ResolveReportDraft(ctx, issuer, draft, evidence.DraftLineage{
		AttemptID:        attempt.ID,
		DiagnosisRunID:   spec.RunID,
		RepositoryID:     spec.Lineage.RepositoryID,
		SnapshotID:       spec.Lineage.SnapshotID,
		CodeIndexBuildID: spec.Lineage.CodeIndexBuildID,
	})
	if report == nil {
		return &evidence.DiagnosisReportData{}
	}
	return report
}
