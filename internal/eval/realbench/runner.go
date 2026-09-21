package realbench

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/agent"
	codeintel "repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/indexing"
	"repolens/internal/llm"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/provider"
	"repolens/internal/retrieval"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
	"repolens/internal/trace"
)

const productionStrategy = "symbol_bm25_structural"

const (
	e2eNotRequested               = "NOT_REQUESTED"
	e2eNotRunProviderUnconfigured = "NOT_RUN_PROVIDER_NOT_CONFIGURED"
	e2eCompleted                  = "E2E_COMPLETED"
	e2eFailure                    = "E2E_FAILURE"
)

type failureClass string

const (
	failureExternalInfra   failureClass = "EXTERNAL_INFRA"
	failureRepolensProduct failureClass = "REPOLENS_PRODUCT"
)

// classifiedError keeps boundary ownership explicit. Git/provider failures
// are external; all failures in RepoLens state, indexing, retrieval, agent,
// and artifact logic are product failures.
type classifiedError struct {
	class failureClass
	stage string
	err   error
}

func (e *classifiedError) Error() string {
	return fmt.Sprintf("%s failed: %v", e.stage, e.err)
}

func (e *classifiedError) Unwrap() error { return e.err }

func externalFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	var existing *classifiedError
	if errors.As(err, &existing) {
		return err
	}
	return &classifiedError{class: failureExternalInfra, stage: stage, err: err}
}

func productFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	var existing *classifiedError
	if errors.As(err, &existing) {
		return err
	}
	return &classifiedError{class: failureRepolensProduct, stage: stage, err: err}
}

func classifyFailure(err error) (status, errorClass string) {
	var classified *classifiedError
	if errors.As(err, &classified) && classified.class == failureExternalInfra {
		return "INFRA_ERROR", string(failureExternalInfra)
	}
	return "PRODUCT_FAILURE", string(failureRepolensProduct)
}

func errorClassFor(err error) string {
	_, class := classifyFailure(err)
	return class
}

func e2eStatusFor(requested, providerConfigured bool) string {
	if !requested {
		return e2eNotRequested
	}
	if !providerConfigured {
		return e2eNotRunProviderUnconfigured
	}
	return e2eFailure
}

type RunOptions struct {
	CaseIDs      []string
	CacheDir     string
	ArtifactRoot string
	RunE2E       bool
}

type RunMetadata struct {
	RunID                 string    `json:"run_id"`
	DatasetVersion        string    `json:"dataset_version"`
	DatasetManifestHash   string    `json:"dataset_manifest_hash"`
	RepoLensGitCommit     string    `json:"repolens_git_commit"`
	RetrievalStrategy     string    `json:"retrieval_strategy"`
	RetrievalVersion      string    `json:"retrieval_version"`
	IndexVersion          string    `json:"index_version"`
	AgentVersion          string    `json:"agent_version,omitempty"`
	PromptVersion         string    `json:"prompt_version,omitempty"`
	Provider              string    `json:"provider,omitempty"`
	Model                 string    `json:"model,omitempty"`
	BaseURLFingerprint    string    `json:"base_url_fingerprint,omitempty"`
	AuthMode              string    `json:"auth_mode,omitempty"`
	AgentConfigHash       string    `json:"agent_config_hash,omitempty"`
	MaxToolCalls          int       `json:"max_tool_calls,omitempty"`
	ToolBudget            int       `json:"tool_budget,omitempty"`
	PreflightStatus       string    `json:"preflight_status,omitempty"`
	Timestamp             time.Time `json:"timestamp"`
	CaseCount             int       `json:"case_count"`
	E2EStatus             string    `json:"e2e_status"`
	WorkingTreeClean      bool      `json:"working_tree_clean"`
	GoVersion             string    `json:"go_version"`
	OS                    string    `json:"os"`
	Arch                  string    `json:"arch"`
	ProviderTimeoutSec    int       `json:"provider_timeout_seconds,omitempty"`
	MaxOutputTokens       int       `json:"max_output_tokens,omitempty"`
	Temperature           float64   `json:"temperature,omitempty"`
	ReasoningEffort       string    `json:"reasoning_effort,omitempty"`
	ResponseFormat        string    `json:"response_format,omitempty"`
	InputPricePerMillion  *float64  `json:"input_price_per_million,omitempty"`
	OutputPricePerMillion *float64  `json:"output_price_per_million,omitempty"`
}

type Prediction struct {
	CaseID            string                   `json:"case_id"`
	Repository        string                   `json:"repository"`
	BuggyCommitSHA    string                   `json:"buggy_commit_sha"`
	SnapshotID        string                   `json:"snapshot_id"`
	Query             string                   `json:"query"`
	RetrievalStrategy string                   `json:"retrieval_strategy"`
	Top10             []retrieval.SearchResult `json:"top10"`
	LatencyMs         int64                    `json:"latency_ms"`
	E2EStatus         string                   `json:"e2e_status"`
}

type CaseStatus struct {
	CaseID                string  `json:"case_id"`
	Repository            string  `json:"repository"`
	BuggyCommitSHA        string  `json:"buggy_commit_sha"`
	Status                string  `json:"status"`
	ErrorClass            string  `json:"error_class,omitempty"`
	ErrorCode             string  `json:"error_code,omitempty"`
	Error                 string  `json:"error,omitempty"`
	HitAt5                bool    `json:"hit_at_5"`
	HitAt10               bool    `json:"hit_at_10"`
	ReciprocalRank        float64 `json:"reciprocal_rank"`
	LatencyMs             int64   `json:"latency_ms"`
	E2EStatus             string  `json:"e2e_status"`
	ExecutionStatus       string  `json:"execution_status"`
	ReportStatus          string  `json:"report_status,omitempty"`
	CitationIntegrityGate string  `json:"citation_integrity_gate,omitempty"`
	E2ELatencyMs          int64   `json:"e2e_latency_ms,omitempty"`
	FinishReason          string  `json:"finish_reason,omitempty"`
}

type Metrics struct {
	TotalCases           int     `json:"total_cases"`
	CompletedCases       int     `json:"completed_cases"`
	InfraErrors          int     `json:"infra_errors"`
	ProductFailures      int     `json:"product_failures"`
	EvaluatedCases       int     `json:"evaluated_cases"`
	HitAt5Count          int     `json:"hit_at_5_count"`
	HitAt10Count         int     `json:"hit_at_10_count"`
	HitAt5               float64 `json:"hit_at_5"`
	HitAt10              float64 `json:"hit_at_10"`
	MRR                  float64 `json:"mrr"`
	CitationStatus       string  `json:"citation_status"`
	CitationGateFailures int     `json:"citation_gate_failures"`
	RootCauseStatus      string  `json:"root_cause_status"`
}

type RunResult struct {
	Metadata RunMetadata  `json:"metadata"`
	Cases    []CaseStatus `json:"cases"`
	Metrics  Metrics      `json:"metrics"`
	RunDir   string       `json:"-"`
}

type analysisQualityArtifact struct {
	FilesTotal          int      `json:"files_total"`
	FilesParsed         int      `json:"files_parsed"`
	FilesFailed         int      `json:"files_failed"`
	ParseRate           float64  `json:"parse_rate"`
	PackagesTotal       int      `json:"packages_total"`
	PackagesTypechecked int      `json:"packages_typechecked"`
	PackagesFailed      int      `json:"packages_failed"`
	TypecheckRate       float64  `json:"typecheck_rate"`
	SymbolsTotal        int      `json:"symbols_total"`
	SemanticRelations   int      `json:"semantic_relations"`
	SyntacticRelations  int      `json:"syntactic_relations"`
	HeuristicRelations  int      `json:"heuristic_relations"`
	UnresolvedRelations int      `json:"unresolved_relations"`
	SymlinksSkipped     int      `json:"symlinks_skipped"`
	RelatedTestsFound   int      `json:"related_tests_found"`
	Warnings            []string `json:"warnings"`
}

type analysisQualityRow struct {
	CaseID string
	analysisQualityArtifact
}

// E2EMetrics is deliberately an evidence artifact, not an accuracy claim.
// Provider fields that are absent from a response are represented explicitly
// as NOT_REPORTED instead of being confused with a reported zero.
type E2EMetrics struct {
	Status                  string      `json:"e2e_status"`
	RootCauseGrade          string      `json:"root_cause_grade"`
	CitationTotal           int         `json:"citation_total"`
	CitationValid           int         `json:"citation_valid"`
	CitationInvalid         int         `json:"citation_invalid"`
	CitationValidityRate    float64     `json:"citation_validity_rate"`
	ReportStatus            string      `json:"report_status"`
	CitationIntegrityGate   string      `json:"citation_integrity_gate"`
	EvidenceItemsIssued     int         `json:"evidence_items_issued"`
	EvidenceItemsReferenced int         `json:"evidence_items_referenced"`
	UnknownEvidenceIDs      int         `json:"unknown_evidence_ids"`
	CrossScopeEvidenceIDs   int         `json:"cross_scope_evidence_ids"`
	EvidenceReferenceRate   float64     `json:"evidence_reference_rate"`
	UnsupportedClaims       int         `json:"unsupported_claims"`
	ToolCalls               int         `json:"tool_calls"`
	ToolNames               []string    `json:"tool_names"`
	AgentRounds             int         `json:"agent_rounds"`
	LatencyMs               int64       `json:"latency_ms"`
	InputTokens             int         `json:"input_tokens"`
	OutputTokens            int         `json:"output_tokens"`
	TotalTokens             int         `json:"total_tokens"`
	CachedTokens            interface{} `json:"cached_tokens"`
	ReasoningTokens         interface{} `json:"reasoning_tokens"`
	ReasoningEffort         string      `json:"reasoning_effort"`
	ResponseFormat          string      `json:"response_format"`
	FinishReason            string      `json:"finish_reason,omitempty"`
	ErrorCode               string      `json:"error_code,omitempty"`
	FailureClassification   string      `json:"failure_classification,omitempty"`
	CostStatus              string      `json:"cost_status"`
	EstimatedCostUSD        *float64    `json:"estimated_cost_usd,omitempty"`
	Attempts                int         `json:"attempts"`
}

type e2eAttempt struct {
	Attempt      int    `json:"attempt"`
	Status       string `json:"status"`
	FailureClass string `json:"failure_classification,omitempty"`
	LatencyMs    int64  `json:"latency_ms"`
}

type Runner struct {
	Dataset *Dataset
	Fetcher SnapshotFetcher
}

// SnapshotFetcher separates the production Git path from offline synthetic
// tests. The default implementation only performs a pinned, read-only checkout.
type SnapshotFetcher interface {
	Fetch(ctx context.Context, input Input, sourceDir string) error
}

type gitSnapshotFetcher struct{}

func (gitSnapshotFetcher) Fetch(ctx context.Context, input Input, sourceDir string) error {
	return ensureExactCheckout(ctx, input.Repository.CloneURL, input.BuggyCommitSHA, sourceDir)
}

func NewRunner(dataset *Dataset) *Runner {
	return &Runner{Dataset: dataset, Fetcher: gitSnapshotFetcher{}}
}

func (r *Runner) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	if r == nil || r.Dataset == nil {
		return nil, errors.New("realbench dataset is required")
	}
	if len(opts.CaseIDs) == 0 {
		for _, inputCase := range r.Dataset.Inputs {
			opts.CaseIDs = append(opts.CaseIDs, inputCase.Input.CaseID)
		}
	}
	caseInputs := make([]InputCase, 0, len(opts.CaseIDs))
	seen := make(map[string]bool, len(opts.CaseIDs))
	for _, caseID := range opts.CaseIDs {
		if seen[caseID] {
			return nil, fmt.Errorf("duplicate requested case %s", caseID)
		}
		seen[caseID] = true
		inputCase, ok := r.Dataset.Input(caseID)
		if !ok {
			return nil, fmt.Errorf("case %s is not in dataset", caseID)
		}
		caseInputs = append(caseInputs, inputCase)
	}

	if opts.CacheDir == "" {
		opts.CacheDir = filepath.Join(".cache", "realbench")
	}
	if opts.ArtifactRoot == "" {
		opts.ArtifactRoot = filepath.Join("artifacts", "realbench")
	}
	runID := time.Now().UTC().Format("20060102T150405Z") + "-" + uuid.New().String()[:8]
	runDir := filepath.Join(opts.ArtifactRoot, runID)
	if err := os.MkdirAll(runDir, 0755); err != nil {
		return nil, fmt.Errorf("create run artifact directory: %w", err)
	}
	gitCommit := currentGitCommit()
	e2eStatus := e2eNotRequested
	providerConfig, providerConfigured := loadProviderConfig()
	e2eStatus = e2eStatusFor(opts.RunE2E, providerConfigured)
	guardConfig := agent.DefaultGuardConfig()
	if maxOutputTokens := configuredRealBenchMaxOutputTokens(); maxOutputTokens > 0 {
		guardConfig.MaxOutputTokens = maxOutputTokens
	}
	generationOptions := configuredRealBenchGenerationOptions()
	result := &RunResult{
		RunDir: runDir,
		Metadata: RunMetadata{
			RunID:               runID,
			DatasetVersion:      r.Dataset.Manifest.DatasetVersion,
			DatasetManifestHash: r.Dataset.Manifest.ManifestHash,
			RepoLensGitCommit:   gitCommit,
			RetrievalStrategy:   productionStrategy,
			RetrievalVersion:    codeintelmodel.CurrentRetrievalVersion,
			IndexVersion:        codeintelmodel.CurrentAnalyzerVersion,
			AgentVersion:        diagnosis.CurrentAgentVersion,
			PromptVersion:       diagnosis.CurrentPromptVersion,
			Timestamp:           time.Now().UTC(),
			CaseCount:           len(caseInputs),
			E2EStatus:           e2eStatus,
			WorkingTreeClean:    workingTreeClean(),
			GoVersion:           runtime.Version(),
			OS:                  runtime.GOOS,
			Arch:                runtime.GOARCH,
			ProviderTimeoutSec:  providerTimeoutSeconds(),
			MaxOutputTokens:     guardConfig.MaxOutputTokens,
			Temperature:         0.1,
			ReasoningEffort:     generationOptions.MetadataReasoningEffort(),
			ResponseFormat:      generationOptions.ResponseFormat,
		},
	}
	qualityRows := make([]analysisQualityRow, 0, len(caseInputs))
	inputPrice, outputPrice, pricesConfigured := configuredPrices()
	if pricesConfigured {
		result.Metadata.InputPricePerMillion = &inputPrice
		result.Metadata.OutputPricePerMillion = &outputPrice
	}
	var preflightErr error
	if opts.RunE2E && providerConfigured {
		result.Metadata.Provider = providerConfig.Provider
		result.Metadata.Model = providerConfig.Model
		result.Metadata.BaseURLFingerprint = providerConfig.EndpointFingerprint
		result.Metadata.AuthMode = providerConfig.AuthMode
		result.Metadata.AgentConfigHash = diagnosis.ComputeAgentConfigHashWithGenerationOptions(guardConfig.MaxSteps, guardConfig.MaxToolCalls, guardConfig.MaxSearchCalls, guardConfig.MaxRepeatCalls, 32*1024, guardConfig.MaxToolResultBytes, 1, guardConfig.MaxOutputTokens, providerTimeoutSeconds(), 0, 0.1, generationOptions.ReasoningEffort, generationOptions.ResponseFormat)
		result.Metadata.MaxToolCalls = guardConfig.MaxToolCalls
		result.Metadata.ToolBudget = guardConfig.MaxToolCalls
		preflight, err := runProviderPreflight(ctx, providerConfig, guardConfig, generationOptions)
		preflightErr = err
		if preflightErr != nil {
			result.Metadata.PreflightStatus = "FAIL"
		} else {
			result.Metadata.PreflightStatus = "PASS"
		}
		if err := writeJSON(filepath.Join(runDir, "preflight.json"), preflight); err != nil {
			return nil, fmt.Errorf("write preflight artifact: %w", err)
		}
	}

	for _, inputCase := range caseInputs {
		caseID := inputCase.Input.CaseID
		caseDir := filepath.Join(runDir, "cases", caseID)
		if err := os.MkdirAll(caseDir, 0755); err != nil {
			return nil, fmt.Errorf("create artifact directory for %s: %w", caseID, err)
		}
		status := CaseStatus{
			CaseID:          caseID,
			Repository:      inputCase.Input.Repository.FullName,
			BuggyCommitSHA:  inputCase.Input.BuggyCommitSHA,
			Status:          "PENDING",
			E2EStatus:       e2eStatus,
			ExecutionStatus: e2eStatus,
		}
		started := time.Now()
		fetcher := r.Fetcher
		if fetcher == nil {
			fetcher = gitSnapshotFetcher{}
		}
		workspace, err := prepareProductionWorkspace(ctx, inputCase.Input, caseID, filepath.Join(opts.CacheDir, caseID), caseDir, fetcher)
		if err != nil {
			status.Status, status.ErrorClass = classifyFailure(err)
			status.Error = err.Error()
			status.LatencyMs = time.Since(started).Milliseconds()
			result.Cases = append(result.Cases, status)
			_ = writeJSON(filepath.Join(caseDir, "status.json"), status)
			continue
		}
		quality := makeAnalysisQualityArtifact(workspace.Quality, workspace.RelatedTestsFound)
		qualityRows = append(qualityRows, analysisQualityRow{CaseID: caseID, analysisQualityArtifact: quality})
		if err := writeJSON(filepath.Join(caseDir, "analysis_quality.json"), quality); err != nil {
			workspace.Close()
			return nil, fmt.Errorf("write %s analysis quality: %w", caseID, err)
		}

		searchStarted := time.Now()
		query, top10, searchErr := searchInput(ctx, workspace.Retriever, inputCase.Input, caseID, workspace.CodeIndexBuildID, workspace.RetrievalBuildID)
		status.LatencyMs = time.Since(searchStarted).Milliseconds()
		if searchErr != nil {
			workspace.Close()
			status.Status = "PRODUCT_FAILURE"
			status.ErrorClass = "REPOLENS_PRODUCT"
			status.Error = searchErr.Error()
			result.Cases = append(result.Cases, status)
			_ = writeJSON(filepath.Join(caseDir, "status.json"), status)
			continue
		}
		caseE2EStatus := e2eStatus
		var diagnosisResult *agent.ExecutionResult
		var e2eMetrics *E2EMetrics
		var e2eErr error
		if opts.RunE2E && providerConfigured {
			e2eStarted := time.Now()
			if preflightErr != nil {
				caseE2EStatus = e2eFailure
				e2eErr = preflightErr
				e2eMetrics = failedE2EMetrics(preflightErr)
				e2eMetrics.ReasoningEffort = generationOptions.MetadataReasoningEffort()
				e2eMetrics.ResponseFormat = generationOptions.ResponseFormat
			} else {
				diagnosisResult, e2eMetrics, e2eErr = runE2E(ctx, inputCase.Input, workspace, providerConfig, caseDir, guardConfig, generationOptions)
			}
			status.E2ELatencyMs = time.Since(e2eStarted).Milliseconds()
			if e2eErr != nil {
				caseE2EStatus = e2eFailure
			} else {
				caseE2EStatus = e2eCompleted
			}
			if e2eMetrics != nil {
				status.ReportStatus = e2eMetrics.ReportStatus
				status.CitationIntegrityGate = e2eMetrics.CitationIntegrityGate
			}
		}

		prediction := Prediction{
			CaseID:            caseID,
			Repository:        inputCase.Input.Repository.FullName,
			BuggyCommitSHA:    inputCase.Input.BuggyCommitSHA,
			SnapshotID:        caseID,
			Query:             query,
			RetrievalStrategy: productionStrategy,
			Top10:             top10,
			LatencyMs:         status.LatencyMs,
			E2EStatus:         caseE2EStatus,
		}
		if diagnosisResult != nil {
			if err := writeJSON(filepath.Join(caseDir, "diagnosis.json"), diagnosisResult); err != nil {
				workspace.Close()
				return nil, fmt.Errorf("write %s diagnosis: %w", caseID, err)
			}
		}
		// Persist the prediction before loading evaluator-only Ground Truth.
		if err := writeJSON(filepath.Join(caseDir, "prediction.json"), prediction); err != nil {
			workspace.Close()
			return nil, fmt.Errorf("write %s prediction: %w", caseID, err)
		}
		if err := writeJSON(filepath.Join(caseDir, "retrieval_top10.json"), top10); err != nil {
			workspace.Close()
			return nil, fmt.Errorf("write %s retrieval results: %w", caseID, err)
		}

		truth, truthErr := r.Dataset.LoadGroundTruth(caseID)
		if truthErr != nil {
			workspace.Close()
			status.Status = "PRODUCT_FAILURE"
			status.ErrorClass = "REPOLENS_PRODUCT"
			status.Error = truthErr.Error()
			result.Cases = append(result.Cases, status)
			_ = writeJSON(filepath.Join(caseDir, "status.json"), status)
			continue
		}
		if e2eMetrics != nil {
			status.FinishReason = e2eMetrics.FinishReason
			status.ErrorCode = e2eMetrics.ErrorCode
			if diagnosisResult != nil {
				e2eMetrics.RootCauseGrade = gradeRootCause(diagnosisResult.Report, truth)
				e2eMetrics.UnsupportedClaims = countUnsupportedClaims(diagnosisResult.Report)
			}
			if e2eErr != nil {
				e2eMetrics.RootCauseGrade = "Not Scorable"
				e2eMetrics.FailureClassification = errorClassFor(e2eErr)
			}
			if err := writeJSON(filepath.Join(caseDir, "e2e_metrics.json"), e2eMetrics); err != nil {
				workspace.Close()
				return nil, fmt.Errorf("write %s E2E metrics: %w", caseID, err)
			}
		}
		status.HitAt5, status.HitAt10, status.ReciprocalRank = retrievalMetrics(top10, truth.PrimaryFiles)
		status.E2EStatus = caseE2EStatus
		status.ExecutionStatus = caseE2EStatus
		status.Status = "RETRIEVAL_COMPLETED_E2E_NOT_RUN"
		if caseE2EStatus == e2eCompleted {
			status.Status = "RETRIEVAL_AND_E2E_COMPLETED"
		} else if caseE2EStatus == e2eFailure {
			status.Status = "RETRIEVAL_COMPLETED_E2E_FAILURE"
			status.ErrorClass = errorClassFor(e2eErr)
			status.Error = failureSummary(e2eErr)
		}
		result.Cases = append(result.Cases, status)
		workspace.Close()
		if err := writeJSON(filepath.Join(caseDir, "status.json"), status); err != nil {
			return nil, fmt.Errorf("write %s status: %w", caseID, err)
		}
	}
	if opts.RunE2E && providerConfigured {
		if preflightErr != nil {
			result.Metadata.E2EStatus = e2eFailure
		} else {
			result.Metadata.E2EStatus = e2eCompleted
		}
		for _, status := range result.Cases {
			if status.E2EStatus == e2eFailure {
				result.Metadata.E2EStatus = e2eFailure
				break
			}
		}
	}

	result.Metrics = aggregateMetrics(result.Cases)
	if err := writeAnalysisQualitySummary(filepath.Join(runDir, "analysis_quality_summary.csv"), qualityRows); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(runDir, "run.json"), result.Metadata); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(runDir, "metrics.json"), result.Metrics); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(runDir, "report.md"), []byte(renderReport(result)), 0644); err != nil {
		return nil, fmt.Errorf("write report: %w", err)
	}
	return result, nil
}

type productionWorkspace struct {
	Retriever         *retrieval.ProductionRetriever
	CodeIndexBuildID  int64
	RetrievalBuildID  int64
	SnapshotStore     snapshotstore.SnapshotStore
	CodeIndexStore    codeintelstore.Store
	Quality           codeintelmodel.AnalysisQuality
	RelatedTestsFound int
	db                *gorm.DB
}

func (w *productionWorkspace) Close() {
	if w == nil || w.db == nil {
		return
	}
	if sqlDB, err := w.db.DB(); err == nil {
		_ = sqlDB.Close()
	}
}

func prepareProductionWorkspace(ctx context.Context, input Input, snapshotID, cacheDir, artifactDir string, fetcher SnapshotFetcher) (*productionWorkspace, error) {
	snapshotStore := snapshotstore.NewLocalSnapshotStore(cacheDir)
	sourceDir := snapshotStore.GetSourcePath(input.CaseID, snapshotID)
	if err := fetcher.Fetch(ctx, input, sourceDir); err != nil {
		return nil, externalFailure("snapshot fetch", err)
	}

	dbPath := filepath.Join(artifactDir, "state.sqlite")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		return nil, productFailure("open benchmark state database", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		return nil, productFailure("migrate benchmark state database", err)
	}
	ciStore := codeintelstore.NewStore(db)
	buildContext := codeintel.DefaultBuildContext()
	analysis, err := codeintel.NewAnalyzer().Analyze(ctx, sourceDir, buildContext)
	if err != nil {
		return nil, productFailure("CodeIndex analysis", err)
	}
	build := &codeintelmodel.CodeIndexBuild{
		SnapshotID:          input.CaseID,
		ParserVersion:       codeintelmodel.CurrentParserVersion,
		AnalyzerVersion:     codeintelmodel.CurrentAnalyzerVersion,
		SymbolSchemaVersion: codeintelmodel.CurrentSymbolSchemaVersion,
		BuildContextHash:    buildContext.BuildContextHash(),
		ModulePath:          analysis.ModulePath,
		GOOS:                buildContext.GOOS,
		GOARCH:              buildContext.GOARCH,
		BuildTagsHash:       buildContext.BuildTagsHash(),
		Status:              codeintelmodel.BuildStatusBuilding,
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.Create(build).Error; err != nil {
		return nil, productFailure("create CodeIndexBuild", err)
	}
	if err := ciStore.SaveAnalysisResult(ctx, build.ID, analysis); err != nil {
		return nil, productFailure("save CodeIndex", err)
	}

	retrievalBuild := &codeintelmodel.RetrievalBuild{
		CodeIndexBuildID: build.ID,
		Strategy:         productionStrategy,
		RetrievalVersion: codeintelmodel.CurrentRetrievalVersion,
		TokenizerVersion: codeintelmodel.CurrentTokenizerVersion,
		ConfigHash:       "config-v2.1",
		Status:           codeintelmodel.BuildStatusCreated,
		CreatedAt:        time.Now().UTC(),
	}
	if err := db.Create(retrievalBuild).Error; err != nil {
		return nil, productFailure("create RetrievalBuild", err)
	}
	if err := ciStore.MarkRetrievalBuilding(ctx, retrievalBuild.ID); err != nil {
		return nil, productFailure("mark RetrievalBuild building", err)
	}
	idx := bm25.NewIndex(1.2, 0.75)
	for _, symbol := range analysis.Symbols {
		content := fmt.Sprintf("%s %s %s %s %s", symbol.Name, symbol.QualifiedName, symbol.ReceiverCanonical, symbol.Signature, symbol.Doc)
		idx.AddDocument(bm25.Document{
			FilePath:      symbol.FilePath,
			StartLine:     symbol.StartLine,
			EndLine:       symbol.EndLine,
			Content:       content,
			SymbolKeyHash: symbol.SymbolKeyHash,
			SymbolName:    symbol.Name,
			Kind:          string(symbol.Kind),
		})
	}
	idx.Build()
	indexRoot := filepath.Join(artifactDir, "indexes")
	artifactPath, artifactHash, err := artifact.NewPublisher(indexRoot).Publish(retrievalBuild.ID, "realbench", productionStrategy, idx)
	if err != nil {
		return nil, productFailure("publish Retrieval artifact", err)
	}
	if err := ciStore.CompleteRetrievalBuild(ctx, retrievalBuild.ID, artifactPath, artifactHash, idx.TotalDocs); err != nil {
		return nil, productFailure("finalize RetrievalBuild", err)
	}
	return &productionWorkspace{
		Retriever:         retrieval.NewProductionRetriever(ciStore, indexRoot),
		CodeIndexBuildID:  build.ID,
		RetrievalBuildID:  retrievalBuild.ID,
		SnapshotStore:     snapshotStore,
		CodeIndexStore:    ciStore,
		Quality:           analysis.Quality,
		RelatedTestsFound: len(analysis.RelatedTests),
		db:                db,
	}, nil
}

type providerConfig struct {
	Provider            string
	APIKey              string
	BaseURL             string
	Model               string
	AuthMode            string
	TimeoutSeconds      int
	IsDemo              bool
	EndpointFingerprint string
}

type realBenchGenerationOptions struct {
	ReasoningEffort string
	ResponseFormat  string
}

func configuredRealBenchGenerationOptions() realBenchGenerationOptions {
	responseFormat := strings.ToLower(strings.TrimSpace(os.Getenv("REPOLENS_REALBENCH_RESPONSE_FORMAT")))
	if responseFormat != "none" && responseFormat != "json_object" {
		responseFormat = "json_object"
	}
	return realBenchGenerationOptions{
		ReasoningEffort: strings.TrimSpace(os.Getenv("REPOLENS_REALBENCH_REASONING_EFFORT")),
		ResponseFormat:  responseFormat,
	}
}

func (o realBenchGenerationOptions) MetadataReasoningEffort() string {
	if o.ReasoningEffort == "" {
		return "not_requested"
	}
	return o.ReasoningEffort
}

func (o realBenchGenerationOptions) AgentOptions() agent.GenerationOptions {
	var responseFormat *llm.ResponseFormat
	if o.ResponseFormat == "json_object" {
		responseFormat = &llm.ResponseFormat{Type: "json_object"}
	}
	return agent.GenerationOptions{
		ReasoningEffort: o.ReasoningEffort,
		ResponseFormat:  responseFormat,
	}
}

func loadProviderConfig() (providerConfig, bool) {
	config := providerConfig{
		Provider:       os.Getenv("REPOLENS_REALBENCH_PROVIDER"),
		APIKey:         os.Getenv("REPOLENS_REALBENCH_API_KEY"),
		BaseURL:        os.Getenv("REPOLENS_REALBENCH_BASE_URL"),
		Model:          os.Getenv("REPOLENS_REALBENCH_MODEL"),
		AuthMode:       os.Getenv("REPOLENS_REALBENCH_AUTH_MODE"),
		TimeoutSeconds: providerTimeoutSeconds(),
	}
	if config.Provider == "" {
		config.Provider = "AIHubMix"
	}
	if config.BaseURL == "" && config.Model == "" && config.APIKey == "" {
		manager := provider.NewManagerWithAuthModeAndTimeoutAndRetries(
			os.Getenv("PROVIDER_SECRET_PATH"),
			os.Getenv("REPOLENS_PROVIDER_BASE_URL"),
			os.Getenv("REPOLENS_PROVIDER_MODEL"),
			os.Getenv("REPOLENS_PROVIDER_API_KEY"),
			os.Getenv("REPOLENS_PROVIDER_TYPE"),
			os.Getenv("REPOLENS_PROVIDER_AUTH_MODE"),
			time.Duration(providerTimeoutSeconds())*time.Second,
			0,
		)
		stored, err := manager.GetSecretConfig()
		if err == nil && stored != nil {
			config.BaseURL = stored.BaseURL
			config.Model = stored.Model
			config.APIKey = stored.APIKey
			config.AuthMode = stored.AuthMode
			config.IsDemo = stored.IsDemo
		}
	}
	config.AuthMode = normalizeAuthMode(config.AuthMode)
	normalized, err := provider.NormalizeBaseURL(config.BaseURL)
	if err == nil {
		config.BaseURL = normalized
		config.EndpointFingerprint = provider.ComputeEndpointFingerprint(normalized)
	}
	authConfigured := config.AuthMode == "none" || config.APIKey != ""
	return config, config.BaseURL != "" && config.Model != "" && authConfigured && !config.IsDemo
}

type providerPreflight struct {
	Provider              string `json:"provider"`
	Model                 string `json:"model"`
	Configured            bool   `json:"is_configured"`
	Demo                  bool   `json:"is_demo"`
	ReasoningEffort       string `json:"reasoning_effort"`
	ResponseFormat        string `json:"response_format"`
	NormalCompletion      string `json:"normal_completion"`
	ToolCalling           string `json:"tool_calling"`
	AgentLoop             string `json:"agent_loop"`
	StructuredReport      string `json:"structured_report"`
	AgentFinishReason     string `json:"agent_finish_reason,omitempty"`
	AgentPromptTokens     int    `json:"agent_prompt_tokens,omitempty"`
	AgentCompletionTokens int    `json:"agent_completion_tokens,omitempty"`
	AgentReasoningTokens  int    `json:"agent_reasoning_tokens,omitempty"`
	AgentToolCalls        int    `json:"agent_tool_calls,omitempty"`
	AgentRounds           int    `json:"agent_rounds,omitempty"`
	AgentStructuredReport bool   `json:"agent_structured_report"`
	AgentErrorCode        string `json:"agent_error_code,omitempty"`
	AgentFailureClass     string `json:"agent_failure_class,omitempty"`
}

func runProviderPreflight(ctx context.Context, config providerConfig, guardConfig agent.GuardConfig, generationOptions realBenchGenerationOptions) (providerPreflight, error) {
	result := providerPreflight{
		Provider:        config.Provider,
		Model:           config.Model,
		Configured:      true,
		Demo:            config.IsDemo,
		ReasoningEffort: generationOptions.MetadataReasoningEffort(),
		ResponseFormat:  generationOptions.ResponseFormat,
	}
	if config.IsDemo {
		return result, productFailure("provider preflight", errors.New("demo provider is not allowed for formal E2E"))
	}
	client := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout(
		config.APIKey,
		config.BaseURL,
		config.Model,
		config.AuthMode,
		time.Duration(config.TimeoutSeconds)*time.Second,
	)
	temperature := 0.1
	if _, err := client.Generate(ctx, llm.GenerateRequest{
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "Reply with exactly OK."}},
		Temperature: &temperature,
	}); err != nil {
		return result, externalFailure("preflight normal completion", err)
	}
	result.NormalCompletion = "PASS"

	toolDefinition := llm.ToolDefinition{
		Type: "function",
		Function: llm.FunctionDef{
			Name:        "repolens_preflight_echo",
			Description: "Return the supplied text for a tool-calling round-trip check.",
			Parameters: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{"text": map[string]interface{}{"type": "string"}},
				"required":   []string{"text"},
			},
		},
	}
	toolResponse, err := client.Generate(ctx, llm.GenerateRequest{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "Call repolens_preflight_echo exactly once with text ping."}},
		Tools:    []llm.ToolDefinition{toolDefinition}, Temperature: &temperature,
	})
	if err != nil {
		return result, externalFailure("preflight tool calling", err)
	}
	if len(toolResponse.Message.ToolCalls) == 0 {
		return result, productFailure("preflight tool calling", errors.New("provider did not return a tool call"))
	}
	toolCall := toolResponse.Message.ToolCalls[0]
	if toolCall.Function.Name != toolDefinition.Function.Name {
		return result, productFailure("preflight tool calling", fmt.Errorf("provider selected unexpected tool %q", toolCall.Function.Name))
	}
	if _, err := client.Generate(ctx, llm.GenerateRequest{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Call repolens_preflight_echo exactly once with text ping."},
			toolResponse.Message,
			{Role: llm.RoleTool, ToolCallID: toolCall.ID, Content: "ping"},
		},
		Tools: []llm.ToolDefinition{toolDefinition}, Temperature: &temperature,
	}); err != nil {
		return result, externalFailure("preflight tool round trip", err)
	}
	result.ToolCalling = "PASS"

	loop := agent.NewAgentLoop(classifiedProvider{Provider: client}, agent.NewToolRegistry(), nil, guardConfig)
	loop.WithGenerationOptions(generationOptions.AgentOptions())
	loopResult, err := loop.Run(ctx, &diagnosis.DiagnosisRun{
		ID: "realbench-preflight", RepositoryID: "preflight", SnapshotID: "preflight",
		IssueTitle: "preflight structured report", IssueDescription: "Return a concise diagnosis report.",
		Temperature: 0.1, ModelName: config.Model,
	}, &diagnosis.DiagnosisAttempt{ID: "realbench-preflight-attempt"})
	if loopResult != nil {
		result.AgentFinishReason = loopResult.FinishReason
		result.AgentPromptTokens = loopResult.PromptTokens
		result.AgentCompletionTokens = loopResult.CompletionTokens
		result.AgentReasoningTokens = loopResult.ReasoningTokens
		result.AgentToolCalls = loopResult.ToolCallsCount
		result.AgentRounds = loopResult.AgentRounds
		result.AgentStructuredReport = loopResult.StructuredReport
	}
	if err != nil {
		result.AgentErrorCode = e2eErrorCode(err)
		result.AgentFailureClass = errorClassFor(err)
		return result, productOrExternalAgentFailure(err)
	}
	result.AgentLoop = "PASS"
	if loopResult == nil || loopResult.Report == nil {
		return result, productFailure("preflight structured report", errors.New("agent returned no report"))
	}
	if !loopResult.StructuredReport {
		return result, productFailure("preflight structured report", errors.New("agent response was not structured JSON"))
	}
	result.StructuredReport = "PASS"
	return result, nil
}

type classifiedProvider struct {
	llm.Provider
}

func executionProgress(err error) *agent.ExecutionResult {
	var executionErr *agent.ExecutionError
	if errors.As(err, &executionErr) {
		return executionErr.Progress
	}
	return nil
}

func (p classifiedProvider) Generate(ctx context.Context, request llm.GenerateRequest) (llm.GenerateResponse, error) {
	response, err := p.Provider.Generate(ctx, request)
	if err != nil {
		return llm.GenerateResponse{}, externalFailure("provider Generate", err)
	}
	return response, nil
}

func runE2E(ctx context.Context, input Input, workspace *productionWorkspace, config providerConfig, caseDir string, guardConfig agent.GuardConfig, generationOptions realBenchGenerationOptions) (*agent.ExecutionResult, *E2EMetrics, error) {
	var lastResult *agent.ExecutionResult
	var lastMetrics *E2EMetrics
	var lastErr error
	attempts := make([]e2eAttempt, 0, 3)
	for attempt := 1; attempt <= 3; attempt++ {
		started := time.Now()
		result, metrics, err := runE2EOnce(ctx, input, workspace, config, caseDir, attempt, started, guardConfig, generationOptions)
		if metrics == nil {
			metrics = &E2EMetrics{Status: e2eFailure, RootCauseGrade: "Not Scorable", CostStatus: costStatus()}
		}
		metrics.Attempts = attempt
		lastResult, lastMetrics, lastErr = result, metrics, err
		attemptRecord := e2eAttempt{Attempt: attempt, Status: e2eCompleted, LatencyMs: time.Since(started).Milliseconds()}
		if err != nil {
			attemptRecord.Status = e2eFailure
			attemptRecord.FailureClass = errorClassFor(err)
		}
		attempts = append(attempts, attemptRecord)
		_ = writeJSON(filepath.Join(caseDir, "e2e_attempts.json"), attempts)
		if err == nil {
			return result, metrics, nil
		}
		if errorClassFor(err) != string(failureExternalInfra) || attempt == 3 {
			return result, metrics, err
		}
	}
	return lastResult, lastMetrics, lastErr
}

func runE2EOnce(ctx context.Context, input Input, workspace *productionWorkspace, config providerConfig, caseDir string, attemptNo int, started time.Time, guardConfig agent.GuardConfig, generationOptions realBenchGenerationOptions) (*agent.ExecutionResult, *E2EMetrics, error) {
	providerClient := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout(
		config.APIKey,
		config.BaseURL,
		config.Model,
		config.AuthMode,
		time.Duration(config.TimeoutSeconds)*time.Second,
	)
	provider := classifiedProvider{Provider: providerClient}
	collector := newTraceCollector()
	executor := agent.NewAgentRuntimeExecutor(
		provider,
		workspace.Retriever,
		workspace.SnapshotStore,
		collector,
		agent.DefaultGuardConfig(),
	)
	executor.WithGenerationOptions(generationOptions.AgentOptions())
	executor.WithCodeIntelStore(workspace.CodeIndexStore)
	evidenceStore := evidence.NewEvidenceStore(workspace.db)
	evidenceIssuer := evidence.NewEvidenceIssuerWithStore(workspace.SnapshotStore, evidenceStore)
	executor.WithEvidenceIssuer(evidenceIssuer)
	run := buildAgentRun(input, workspace, config.Model, guardConfig.MaxOutputTokens)
	attempt := &diagnosis.DiagnosisAttempt{ID: uuid.New().String()}
	result, err := executor.Execute(ctx, run, attempt)
	if err != nil {
		progress := executionProgress(err)
		metrics := metricsFromExecution(progress, collector, time.Since(started).Milliseconds())
		metrics.ReasoningEffort = generationOptions.MetadataReasoningEffort()
		metrics.ResponseFormat = generationOptions.ResponseFormat
		metrics.Status = e2eFailure
		metrics.FailureClassification = errorClassFor(err)
		metrics.ErrorCode = e2eErrorCode(err)
		metrics.CostStatus = costStatus()
		_ = writeJSON(filepath.Join(caseDir, fmt.Sprintf("agent_trace_attempt_%d.json", attemptNo)), collector.Steps())
		_ = writeJSON(filepath.Join(caseDir, "agent_trace.json"), collector.Steps())
		return progress, metrics, productOrExternalAgentFailure(err)
	}
	if result == nil {
		err = productFailure("Agent runtime", errors.New("empty execution result"))
		metrics := metricsFromExecution(nil, collector, time.Since(started).Milliseconds())
		metrics.ReasoningEffort = generationOptions.MetadataReasoningEffort()
		metrics.ResponseFormat = generationOptions.ResponseFormat
		metrics.Status = e2eFailure
		metrics.FailureClassification = errorClassFor(err)
		metrics.CostStatus = costStatus()
		return nil, metrics, err
	}
	metrics := metricsFromExecution(result, collector, time.Since(started).Milliseconds())
	metrics.ReasoningEffort = generationOptions.MetadataReasoningEffort()
	metrics.ResponseFormat = generationOptions.ResponseFormat
	metrics.Status = e2eCompleted
	metrics.CostStatus = costStatus()
	metrics.EstimatedCostUSD = estimateCost(result)
	citations := []evidence.Citation{}
	issuedItems, _ := evidenceIssuer.ListByAttempt(ctx, attempt.ID)
	metrics.EvidenceItemsIssued = len(issuedItems)
	if result.Report != nil {
		citations = flattenCitations(result.Report)
		validator := evidence.NewCitationValidator(workspace.SnapshotStore)
		valid := 0
		for i := range citations {
			if citations[i].EvidenceID == "" && citations[i].FilePath != "" {
				validator.Validate(ctx, input.CaseID, input.CaseID, &citations[i])
			}
			if citations[i].ValidationStatus == evidence.CitationValid {
				valid++
			}
		}
		metrics.CitationTotal = len(citations)
		metrics.CitationValid = valid
		metrics.CitationInvalid = len(citations) - valid
		metrics.CitationValidityRate = citationValidityRate(len(citations), valid)
		referencedIDs := make(map[string]struct{})
		for _, citation := range citations {
			if citation.EvidenceID == "" {
				continue
			}
			if _, seen := referencedIDs[citation.EvidenceID]; seen {
				continue
			}
			referencedIDs[citation.EvidenceID] = struct{}{}
			metrics.EvidenceItemsReferenced++
			if citation.ValidationStatus != evidence.CitationInvalid {
				continue
			}
			switch citation.ValidationError {
			case "EVIDENCE_NOT_FOUND":
				if foreign, findErr := evidenceIssuer.FindByID(ctx, citation.EvidenceID); findErr == nil && foreign != nil && foreign.AttemptID != attempt.ID {
					metrics.CrossScopeEvidenceIDs++
				} else {
					metrics.UnknownEvidenceIDs++
				}
			case "EVIDENCE_ATTEMPT_MISMATCH", "EVIDENCE_LINEAGE_MISMATCH":
				metrics.CrossScopeEvidenceIDs++
			}
		}
		metrics.EvidenceReferenceRate = evidenceReferenceRate(metrics.EvidenceItemsIssued, metrics.EvidenceItemsReferenced)
	} else if err := writeJSON(filepath.Join(caseDir, "citation_result.json"), map[string]interface{}{
		"total": 0, "valid": 0, "invalid": 0, "validity_rate": 0, "evidence_items_issued": metrics.EvidenceItemsIssued, "evidence_items_referenced": 0, "citations": []evidence.Citation{},
	}); err != nil {
		return nil, metrics, productFailure("write citation result", err)
	}
	_ = writeJSON(filepath.Join(caseDir, fmt.Sprintf("agent_trace_attempt_%d.json", attemptNo)), collector.Steps())
	_ = writeJSON(filepath.Join(caseDir, "agent_trace.json"), collector.Steps())
	quality, qualityErr := evidence.ClassifyReport(result.Report, result.StructuredReport)
	if qualityErr == nil {
		metrics.ReportStatus = string(quality.Status)
	} else {
		metrics.ReportStatus = string(evidence.ReportInvalid)
	}
	if metrics.CitationInvalid == 0 {
		metrics.CitationIntegrityGate = "PASSED"
	} else {
		metrics.CitationIntegrityGate = "FAILED"
	}
	if err := writeJSON(filepath.Join(caseDir, "citation_result.json"), map[string]interface{}{
		"total": metrics.CitationTotal, "valid": metrics.CitationValid, "invalid": metrics.CitationInvalid,
		"validity_rate": metrics.CitationValidityRate, "report_status": metrics.ReportStatus,
		"citation_integrity_gate": metrics.CitationIntegrityGate,
		"evidence_items_issued":   metrics.EvidenceItemsIssued, "evidence_items_referenced": metrics.EvidenceItemsReferenced,
		"unknown_evidence_ids": metrics.UnknownEvidenceIDs, "cross_scope_evidence_ids": metrics.CrossScopeEvidenceIDs,
		"evidence_reference_rate": metrics.EvidenceReferenceRate, "citations": citations,
	}); err != nil {
		return nil, metrics, productFailure("write citation result", err)
	}
	return result, metrics, nil
}

func productOrExternalAgentFailure(err error) error {
	return productFailure("Agent runtime", err)
}

type traceCollector struct {
	mu    sync.Mutex
	steps []trace.AgentStep
}

func newTraceCollector() *traceCollector {
	return &traceCollector{}
}

func (c *traceCollector) Create(_ context.Context, step *trace.AgentStep) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	copyOfStep := *step
	c.steps = append(c.steps, copyOfStep)
	return nil
}

func (c *traceCollector) ListByAttempt(_ context.Context, attemptID string) ([]trace.AgentStep, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	steps := make([]trace.AgentStep, 0, len(c.steps))
	for _, step := range c.steps {
		if step.AttemptID == attemptID {
			steps = append(steps, step)
		}
	}
	return steps, nil
}

func (c *traceCollector) ListAfterSeq(ctx context.Context, attemptID string, lastSeq int) ([]trace.AgentStep, error) {
	steps, err := c.ListByAttempt(ctx, attemptID)
	if err != nil {
		return nil, err
	}
	filtered := steps[:0]
	for _, step := range steps {
		if step.Seq > lastSeq {
			filtered = append(filtered, step)
		}
	}
	return filtered, nil
}

func (c *traceCollector) Steps() []trace.AgentStep {
	c.mu.Lock()
	defer c.mu.Unlock()
	steps := append([]trace.AgentStep(nil), c.steps...)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Seq < steps[j].Seq })
	return steps
}

func metricsFromExecution(result *agent.ExecutionResult, collector *traceCollector, latencyMs int64) *E2EMetrics {
	metrics := &E2EMetrics{
		Status:                e2eFailure,
		RootCauseGrade:        "Not Scorable",
		ReportStatus:          "NOT_AVAILABLE",
		CitationIntegrityGate: "NOT_RUN",
		ToolNames:             []string{},
		LatencyMs:             latencyMs,
		CachedTokens:          "NOT_REPORTED",
		ReasoningTokens:       "NOT_REPORTED",
		CostStatus:            costStatus(),
	}
	if result != nil {
		metrics.ToolCalls = result.ToolCalls
		metrics.ToolNames = append([]string(nil), result.ToolNames...)
		metrics.AgentRounds = result.AgentRounds
		metrics.InputTokens = result.PromptTokens
		metrics.OutputTokens = result.CompletionTokens
		metrics.TotalTokens = result.PromptTokens + result.CompletionTokens
		metrics.FinishReason = result.FinishReason
		if result.CachedPromptTokens > 0 {
			metrics.CachedTokens = result.CachedPromptTokens
		}
		if result.ReasoningTokens > 0 {
			metrics.ReasoningTokens = result.ReasoningTokens
		}
		return metrics
	}
	for _, step := range collector.Steps() {
		switch step.StepType {
		case trace.StepTypeThinking:
			metrics.AgentRounds++
		case trace.StepTypeToolCall:
			metrics.ToolCalls++
			metrics.ToolNames = append(metrics.ToolNames, step.ToolName)
		}
	}
	return metrics
}

func failedE2EMetrics(err error) *E2EMetrics {
	return &E2EMetrics{
		Status:                e2eFailure,
		RootCauseGrade:        "Not Scorable",
		ReportStatus:          "NOT_AVAILABLE",
		CitationIntegrityGate: "NOT_RUN",
		FailureClassification: errorClassFor(err),
		ErrorCode:             e2eErrorCode(err),
		ToolNames:             []string{},
		CachedTokens:          "NOT_REPORTED",
		ReasoningTokens:       "NOT_REPORTED",
		CostStatus:            costStatus(),
	}
}

func e2eErrorCode(err error) string {
	if errors.Is(err, agent.ErrModelOutputTruncated) {
		return agent.ErrCodeModelOutputTruncated
	}
	return ""
}

func citationValidityRate(total, valid int) float64 {
	if total == 0 {
		return 0
	}
	return float64(valid) / float64(total)
}

func evidenceReferenceRate(issued, referenced int) float64 {
	if issued == 0 {
		return 0
	}
	return float64(referenced) / float64(issued)
}

func countUnsupportedClaims(report *evidence.DiagnosisReportData) int {
	if report == nil {
		return 0
	}
	unsupported := 0
	for _, finding := range report.Findings {
		supported := false
		for _, citation := range finding.Citations {
			if citation.ValidationStatus == evidence.CitationValid {
				supported = true
				break
			}
		}
		if !supported {
			unsupported++
		}
	}
	if len(report.Findings) == 0 && strings.TrimSpace(report.RootCause) != "" {
		unsupported = 1
	}
	return unsupported
}

func gradeRootCause(report *evidence.DiagnosisReportData, truth GroundTruth) string {
	if report == nil {
		return "Not Scorable"
	}
	actual := strings.ToLower(report.Summary + " " + report.RootCause)
	for _, finding := range report.Findings {
		actual += " " + strings.ToLower(finding.Title+" "+finding.Reasoning)
	}
	expectedTokens := significantTokens(truth.ExpectedRootCause)
	if len(expectedTokens) == 0 {
		return "Not Scorable"
	}
	matched := 0
	for token := range expectedTokens {
		if strings.Contains(actual, token) {
			matched++
		}
	}
	overlap := float64(matched) / float64(len(expectedTokens))
	fileMatch := false
	for _, finding := range report.Findings {
		for _, citation := range finding.Citations {
			for _, file := range truth.PrimaryFiles {
				if normalizePath(citation.FilePath) == normalizePath(file) {
					fileMatch = true
				}
			}
		}
	}
	// Root-cause grading measures the diagnosis text. Citation integrity is a
	// separate gate; an invalid or stale handle must not turn an otherwise
	// correct explanation into an Incorrect grade.
	if overlap >= 0.6 {
		return "Correct"
	}
	if fileMatch || overlap >= 0.3 {
		return "Partial"
	}
	return "Incorrect"
}

func significantTokens(text string) map[string]bool {
	result := make(map[string]bool)
	for _, token := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'))
	}) {
		if len(token) >= 5 {
			result[token] = true
		}
	}
	return result
}

func costStatus() string {
	_, _, ok := configuredPrices()
	if ok {
		return "CONFIGURED"
	}
	return "NOT_CONFIGURED"
}

func configuredPrices() (float64, float64, bool) {
	input, inputErr := strconv.ParseFloat(os.Getenv("REPOLENS_REALBENCH_INPUT_PRICE_PER_MILLION"), 64)
	output, outputErr := strconv.ParseFloat(os.Getenv("REPOLENS_REALBENCH_OUTPUT_PRICE_PER_MILLION"), 64)
	if inputErr != nil || outputErr != nil || input < 0 || output < 0 {
		return 0, 0, false
	}
	return input, output, true
}

func estimateCost(result *agent.ExecutionResult) *float64 {
	if result == nil {
		return nil
	}
	inputPrice, outputPrice, ok := configuredPrices()
	if !ok {
		return nil
	}
	cost := float64(result.PromptTokens)*inputPrice/1_000_000 + float64(result.CompletionTokens)*outputPrice/1_000_000
	return &cost
}

func failureSummary(err error) string {
	if err == nil {
		return ""
	}
	var classified *classifiedError
	if errors.As(err, &classified) {
		return fmt.Sprintf("%s failure at %s", classified.class, classified.stage)
	}
	return "REPOLENS_PRODUCT failure"
}

func normalizeAuthMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "none") {
		return "none"
	}
	return "bearer"
}

func buildRetrievalQuery(input Input) string {
	return strings.TrimSpace(strings.Join([]string{input.IssueTitle, input.IssueDescription, input.ErrorLog}, "\n"))
}

func searchInput(ctx context.Context, retriever retrieval.Retriever, input Input, snapshotID string, codeIndexBuildID, retrievalBuildID int64) (string, []retrieval.SearchResult, error) {
	query := buildRetrievalQuery(input)
	results, err := retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: snapshotID, CodeIndexBuildID: codeIndexBuildID, RetrievalBuildID: retrievalBuildID,
		Query: query, TopK: 10,
	})
	return query, results, err
}

func buildAgentRun(input Input, workspace *productionWorkspace, model string, maxOutputTokens int) *diagnosis.DiagnosisRun {
	return &diagnosis.DiagnosisRun{
		ID: uuid.New().String(), RepositoryID: input.CaseID, SnapshotID: input.CaseID,
		CodeIndexBuildID: workspace.CodeIndexBuildID, RetrievalBuildID: workspace.RetrievalBuildID,
		IssueTitle: input.IssueTitle, IssueDescription: input.IssueDescription, ErrorLog: input.ErrorLog,
		Temperature: 0.1, ModelName: model, PromptVersion: diagnosis.CurrentPromptVersion, AgentVersion: diagnosis.CurrentAgentVersion,
		MaxOutputTokens: maxOutputTokens,
	}
}

func flattenCitations(report *evidence.DiagnosisReportData) []evidence.Citation {
	var citations []evidence.Citation
	for _, finding := range report.Findings {
		citations = append(citations, finding.Citations...)
	}
	return citations
}

func ensureExactCheckout(ctx context.Context, cloneURL, commitSHA, sourceDir string) error {
	cloner := indexing.NewSafeGitCloner([]string{"github.com"}, 50, 10*time.Minute)
	if err := cloner.ValidateGitURL(cloneURL); err != nil {
		return fmt.Errorf("repository validation failed: %w", err)
	}
	if _, err := os.Stat(filepath.Join(sourceDir, ".git")); err == nil {
		actual, revErr := runGit(ctx, sourceDir, "rev-parse", "HEAD")
		if revErr == nil && strings.TrimSpace(actual) == commitSHA {
			return nil
		}
		return fmt.Errorf("cached snapshot exists but is not buggy SHA %s", commitSHA)
	}
	if _, err := os.Stat(sourceDir); err == nil {
		return fmt.Errorf("snapshot target exists without a usable git checkout: %s", sourceDir)
	}
	if err := os.MkdirAll(filepath.Dir(sourceDir), 0755); err != nil {
		return fmt.Errorf("create snapshot parent: %w", err)
	}
	if _, err := runGit(ctx, "", "-c", "core.hooksPath=/dev/null", "clone", "--no-checkout", "--filter=blob:none", "--no-tags", "--depth", "1", cloneURL, sourceDir); err != nil {
		return fmt.Errorf("clone repository: %w", err)
	}
	if _, err := runGit(ctx, sourceDir, "fetch", "--depth", "1", "origin", commitSHA); err != nil {
		return fmt.Errorf("fetch buggy SHA: %w", err)
	}
	if _, err := runGit(ctx, sourceDir, "checkout", "--detach", "--force", commitSHA); err != nil {
		return fmt.Errorf("checkout buggy SHA: %w", err)
	}
	actual, err := runGit(ctx, sourceDir, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(actual) != commitSHA {
		return fmt.Errorf("exact SHA verification failed: got %q want %s", strings.TrimSpace(actual), commitSHA)
	}
	return nil
}

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	fullArgs := append([]string{}, args...)
	if dir != "" {
		fullArgs = append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null"}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_LFS_SKIP_SMUDGE=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("git %s: %w: %s", strings.Join(fullArgs, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func currentGitCommit() string {
	output, err := runGit(context.Background(), "", "rev-parse", "HEAD")
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(output)
}

func workingTreeClean() bool {
	output, err := runGit(context.Background(), "", "status", "--porcelain")
	return err == nil && strings.TrimSpace(output) == ""
}

func providerTimeoutSeconds() int {
	value, err := strconv.Atoi(os.Getenv("REPOLENS_PROVIDER_TIMEOUT_SECONDS"))
	if err != nil || value <= 0 {
		return 60
	}
	return value
}

func configuredRealBenchMaxOutputTokens() int {
	value, err := strconv.Atoi(os.Getenv("REPOLENS_REALBENCH_MAX_OUTPUT_TOKENS"))
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func retrievalMetrics(results []retrieval.SearchResult, relevantFiles []string) (bool, bool, float64) {
	relevant := make(map[string]bool, len(relevantFiles))
	for _, file := range relevantFiles {
		relevant[normalizePath(file)] = true
	}
	first := 0
	var hit5, hit10 bool
	for i, result := range results {
		if !relevant[normalizePath(result.Path)] {
			continue
		}
		if first == 0 {
			first = i + 1
		}
		if i < 5 {
			hit5 = true
		}
		if i < 10 {
			hit10 = true
		}
	}
	if first == 0 {
		return hit5, hit10, 0
	}
	return hit5, hit10, 1 / float64(first)
}

func normalizePath(path string) string {
	return strings.ToLower(strings.ReplaceAll(path, "\\", "/"))
}

func aggregateMetrics(cases []CaseStatus) Metrics {
	metrics := Metrics{TotalCases: len(cases), CitationStatus: "NOT_RUN_RETRIEVAL_ONLY", RootCauseStatus: e2eNotRequested}
	for _, status := range cases {
		switch status.E2EStatus {
		case e2eNotRunProviderUnconfigured:
			metrics.RootCauseStatus = e2eNotRunProviderUnconfigured
		case e2eCompleted:
			metrics.RootCauseStatus = e2eCompleted
		case e2eFailure:
			metrics.RootCauseStatus = e2eFailure
		}
		switch status.Status {
		case "RETRIEVAL_COMPLETED_E2E_NOT_RUN", "RETRIEVAL_AND_E2E_COMPLETED", "RETRIEVAL_COMPLETED_E2E_FAILURE":
			metrics.CompletedCases++
			metrics.EvaluatedCases++
			if status.HitAt5 {
				metrics.HitAt5Count++
			}
			if status.HitAt10 {
				metrics.HitAt10Count++
			}
			metrics.MRR += status.ReciprocalRank
		case "INFRA_ERROR":
			metrics.InfraErrors++
		case "PRODUCT_FAILURE":
			metrics.ProductFailures++
		}
		if status.Status == "RETRIEVAL_COMPLETED_E2E_FAILURE" {
			if status.ErrorClass == string(failureExternalInfra) {
				metrics.InfraErrors++
			} else {
				metrics.ProductFailures++
			}
		}
		if status.CitationIntegrityGate == "FAILED" {
			metrics.CitationGateFailures++
		}
	}
	if metrics.CitationGateFailures > 0 {
		metrics.CitationStatus = "FAILED"
	} else if metrics.RootCauseStatus == e2eCompleted {
		metrics.CitationStatus = "PASSED"
	}
	if metrics.EvaluatedCases > 0 {
		metrics.HitAt5 = float64(metrics.HitAt5Count) / float64(metrics.EvaluatedCases)
		metrics.HitAt10 = float64(metrics.HitAt10Count) / float64(metrics.EvaluatedCases)
		metrics.MRR /= float64(metrics.EvaluatedCases)
	}
	return metrics
}

func renderReport(result *RunResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# RealBench v1 Retrieval Baseline\n\n")
	fmt.Fprintf(&b, "- Dataset: `%s`\n- Cases: %d\n- Retrieval: `%s`\n- E2E: `%s`\n\n", result.Metadata.DatasetVersion, result.Metrics.TotalCases, result.Metadata.RetrievalStrategy, result.Metadata.E2EStatus)
	fmt.Fprintf(&b, "## Summary\n\n")
	fmt.Fprintf(&b, "- Completed: %d\n- Infra Errors: %d\n- Product Failures: %d\n- Hit@5: %d/%d (%.1f%%)\n- Hit@10: %d/%d (%.1f%%)\n- MRR: %.3f\n\n", result.Metrics.CompletedCases, result.Metrics.InfraErrors, result.Metrics.ProductFailures, result.Metrics.HitAt5Count, result.Metrics.EvaluatedCases, result.Metrics.HitAt5*100, result.Metrics.HitAt10Count, result.Metrics.EvaluatedCases, result.Metrics.HitAt10*100, result.Metrics.MRR)
	fmt.Fprintf(&b, "## Cases\n\n| Case | Repository | Status | Hit@5 | Hit@10 | RR | E2E |\n|---|---|---|---:|---:|---:|---|\n")
	ordered := append([]CaseStatus(nil), result.Cases...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].CaseID < ordered[j].CaseID })
	for _, status := range ordered {
		fmt.Fprintf(&b, "| %s | %s | %s | %t | %t | %.3f | %s |\n", status.CaseID, status.Repository, status.Status, status.HitAt5, status.HitAt10, status.ReciprocalRank, status.E2EStatus)
	}
	return b.String()
}

func writeJSON(path string, value interface{}) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0644)
}

func makeAnalysisQualityArtifact(quality codeintelmodel.AnalysisQuality, relatedTestsFound int) analysisQualityArtifact {
	parseRate := 0.0
	if quality.FilesTotal > 0 {
		parseRate = float64(quality.FilesParsed) / float64(quality.FilesTotal)
	}
	typecheckRate := 0.0
	if quality.PackagesTotal > 0 {
		typecheckRate = float64(quality.PackagesTypechecked) / float64(quality.PackagesTotal)
	}
	return analysisQualityArtifact{
		FilesTotal:          quality.FilesTotal,
		FilesParsed:         quality.FilesParsed,
		FilesFailed:         quality.FilesFailed,
		ParseRate:           parseRate,
		PackagesTotal:       quality.PackagesTotal,
		PackagesTypechecked: quality.PackagesTypechecked,
		PackagesFailed:      quality.PackagesFailed,
		TypecheckRate:       typecheckRate,
		SymbolsTotal:        quality.SymbolsTotal,
		SemanticRelations:   quality.SemanticRelationsCount,
		SyntacticRelations:  quality.SyntacticRelationsCount,
		HeuristicRelations:  quality.HeuristicRelationsCount,
		UnresolvedRelations: quality.UnresolvedRelationsCount,
		SymlinksSkipped:     quality.SymlinksSkipped,
		RelatedTestsFound:   relatedTestsFound,
		Warnings:            quality.Warnings,
	}
}

func writeAnalysisQualitySummary(path string, rows []analysisQualityRow) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create analysis quality summary: %w", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	if err := writer.Write([]string{
		"case_id", "files_total", "files_parsed", "files_failed", "parse_rate",
		"packages_total", "packages_typechecked", "packages_failed", "typecheck_rate",
		"symbols_total", "semantic_relations", "syntactic_relations", "heuristic_relations",
		"unresolved_relations", "symlinks_skipped", "related_tests_found", "warnings",
	}); err != nil {
		return fmt.Errorf("write analysis quality header: %w", err)
	}
	for _, row := range rows {
		quality := row.analysisQualityArtifact
		if err := writer.Write([]string{
			row.CaseID,
			strconv.Itoa(quality.FilesTotal),
			strconv.Itoa(quality.FilesParsed),
			strconv.Itoa(quality.FilesFailed),
			strconv.FormatFloat(quality.ParseRate, 'f', 6, 64),
			strconv.Itoa(quality.PackagesTotal),
			strconv.Itoa(quality.PackagesTypechecked),
			strconv.Itoa(quality.PackagesFailed),
			strconv.FormatFloat(quality.TypecheckRate, 'f', 6, 64),
			strconv.Itoa(quality.SymbolsTotal),
			strconv.Itoa(quality.SemanticRelations),
			strconv.Itoa(quality.SyntacticRelations),
			strconv.Itoa(quality.HeuristicRelations),
			strconv.Itoa(quality.UnresolvedRelations),
			strconv.Itoa(quality.SymlinksSkipped),
			strconv.Itoa(quality.RelatedTestsFound),
			strings.Join(quality.Warnings, " | "),
		}); err != nil {
			return fmt.Errorf("write analysis quality row %s: %w", row.CaseID, err)
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return fmt.Errorf("flush analysis quality summary: %w", err)
	}
	return nil
}
