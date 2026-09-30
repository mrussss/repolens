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
	"repolens/internal/trace"
)

const (
	ErrCodeExecutionSpecVersionUnsupported = "EXECUTION_SPEC_VERSION_UNSUPPORTED"
	ErrCodeExecutionSpecConfigMismatch     = "EXECUTION_SPEC_CONFIG_MISMATCH"
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
	if err := validateExecutionSpecCompatibility(spec, e.generation); err != nil {
		return nil, err
	}

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

	registry, err := BuildToolRegistry(ToolDependencies{
		Retriever: e.retriever, CodeIntelStore: e.ciStore,
		SnapshotStore: e.storeFS, EvidenceIssuer: e.evidenceIssuer,
	}, spec, attempt.ID)
	if err != nil {
		return nil, err
	}

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
	packetBytes := e.evidenceBytes
	if spec.Budget.MaxEvidencePacketBytes > 0 {
		packetBytes = spec.Budget.MaxEvidencePacketBytes
	}
	generation := GenerationOptions{
		ReasoningEffort: strings.TrimSpace(spec.Generation.ReasoningEffort),
		ResponseFormat:  &llm.ResponseFormat{Type: "json_object"},
	}
	if e.generation != nil {
		// Compatibility validation ties this override to the response format
		// recorded by the frozen AgentConfigHash.
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
				Report: FinalizeReport(ctx, e.evidenceIssuer, res.ReportDraft, spec, attempt), ReportDraft: res.ReportDraft, RawOutput: res.RawOutput,
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
		Report:             FinalizeReport(ctx, e.evidenceIssuer, res.ReportDraft, spec, attempt),
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

func validateExecutionSpecCompatibility(spec diagnosis.DiagnosisExecutionSpec, generationOverride *GenerationOptions) error {
	if spec.Generation.PromptVersion != diagnosis.CurrentPromptVersion || spec.Generation.AgentVersion != diagnosis.CurrentAgentVersion {
		return jobs.NewPermanentError(ErrCodeExecutionSpecVersionUnsupported,
			"frozen diagnosis prompt or Agent version is not supported by this runtime", nil)
	}

	responseFormat := "json_object"
	if generationOverride != nil {
		if generationOverride.ResponseFormat == nil {
			responseFormat = "none"
		} else if generationOverride.ResponseFormat.Type != "json_object" {
			return jobs.NewPermanentError(ErrCodeExecutionSpecConfigMismatch,
				"runtime response format is not supported by the frozen execution contract", nil)
		}
	}
	expectedHash := diagnosis.ComputeAgentConfigHashWithGenerationOptions(
		spec.Budget.MaxAgentRounds,
		spec.Budget.MaxToolCalls,
		spec.Budget.MaxSearchCalls,
		spec.Budget.MaxRepeatCalls,
		spec.Budget.MaxEvidencePacketBytes,
		spec.Budget.MaxToolResultBytes,
		spec.Budget.FinalizationTurns,
		spec.Generation.MaxOutputTokens,
		spec.Provider.TimeoutSeconds,
		spec.Provider.RetryAttempts,
		spec.Generation.Temperature,
		spec.Generation.ReasoningEffort,
		responseFormat,
	)
	if spec.Generation.AgentConfigHash == "" || spec.Generation.AgentConfigHash != expectedHash {
		return jobs.NewPermanentError(ErrCodeExecutionSpecConfigMismatch,
			"frozen diagnosis Agent configuration does not match this runtime", nil)
	}
	return nil
}
