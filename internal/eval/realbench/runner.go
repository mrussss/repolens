package realbench

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	codeintel "repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/indexing"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
)

const productionStrategy = "symbol_bm25_structural"

type RunOptions struct {
	CaseIDs      []string
	CacheDir     string
	ArtifactRoot string
	RunE2E       bool
}

type RunMetadata struct {
	RunID               string    `json:"run_id"`
	DatasetVersion      string    `json:"dataset_version"`
	DatasetManifestHash string    `json:"dataset_manifest_hash"`
	RepoLensGitCommit   string    `json:"repolens_git_commit"`
	RetrievalStrategy   string    `json:"retrieval_strategy"`
	RetrievalVersion    string    `json:"retrieval_version"`
	IndexVersion        string    `json:"index_version"`
	AgentVersion        string    `json:"agent_version,omitempty"`
	PromptVersion       string    `json:"prompt_version,omitempty"`
	Provider            string    `json:"provider,omitempty"`
	Model               string    `json:"model,omitempty"`
	Timestamp           time.Time `json:"timestamp"`
	CaseCount           int       `json:"case_count"`
	E2EStatus           string    `json:"e2e_status"`
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
	CaseID         string  `json:"case_id"`
	Repository     string  `json:"repository"`
	BuggyCommitSHA string  `json:"buggy_commit_sha"`
	Status         string  `json:"status"`
	ErrorClass     string  `json:"error_class,omitempty"`
	Error          string  `json:"error,omitempty"`
	HitAt5         bool    `json:"hit_at_5"`
	HitAt10        bool    `json:"hit_at_10"`
	ReciprocalRank float64 `json:"reciprocal_rank"`
	LatencyMs      int64   `json:"latency_ms"`
	E2EStatus      string  `json:"e2e_status"`
}

type Metrics struct {
	TotalCases      int     `json:"total_cases"`
	CompletedCases  int     `json:"completed_cases"`
	InfraErrors     int     `json:"infra_errors"`
	ProductFailures int     `json:"product_failures"`
	EvaluatedCases  int     `json:"evaluated_cases"`
	HitAt5Count     int     `json:"hit_at_5_count"`
	HitAt10Count    int     `json:"hit_at_10_count"`
	HitAt5          float64 `json:"hit_at_5"`
	HitAt10         float64 `json:"hit_at_10"`
	MRR             float64 `json:"mrr"`
	CitationStatus  string  `json:"citation_status"`
	RootCauseStatus string  `json:"root_cause_status"`
}

type RunResult struct {
	Metadata RunMetadata  `json:"metadata"`
	Cases    []CaseStatus `json:"cases"`
	Metrics  Metrics      `json:"metrics"`
	RunDir   string       `json:"-"`
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
	e2eStatus := "NOT_RUN_PROVIDER_NOT_CONFIGURED"
	if opts.RunE2E {
		e2eStatus = "NOT_RUN_PROVIDER_NOT_CONFIGURED"
	}
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
			AgentVersion:        "v2.1",
			PromptVersion:       "v2.1",
			Timestamp:           time.Now().UTC(),
			CaseCount:           len(caseInputs),
			E2EStatus:           e2eStatus,
		},
	}

	for _, inputCase := range caseInputs {
		caseID := inputCase.Input.CaseID
		caseDir := filepath.Join(runDir, "cases", caseID)
		if err := os.MkdirAll(caseDir, 0755); err != nil {
			return nil, fmt.Errorf("create artifact directory for %s: %w", caseID, err)
		}
		status := CaseStatus{
			CaseID:         caseID,
			Repository:     inputCase.Input.Repository.FullName,
			BuggyCommitSHA: inputCase.Input.BuggyCommitSHA,
			Status:         "PENDING",
			E2EStatus:      e2eStatus,
		}
		started := time.Now()
		fetcher := r.Fetcher
		if fetcher == nil {
			fetcher = gitSnapshotFetcher{}
		}
		workspace, err := prepareProductionWorkspace(ctx, inputCase.Input, caseID, filepath.Join(opts.CacheDir, caseID), caseDir, fetcher)
		if err != nil {
			status.Status = "INFRA_ERROR"
			status.ErrorClass = "EXTERNAL_INFRA"
			status.Error = err.Error()
			status.LatencyMs = time.Since(started).Milliseconds()
			result.Cases = append(result.Cases, status)
			_ = writeJSON(filepath.Join(caseDir, "status.json"), status)
			continue
		}

		query := strings.TrimSpace(strings.Join([]string{inputCase.Input.IssueTitle, inputCase.Input.IssueDescription, inputCase.Input.ErrorLog}, "\n"))
		searchStarted := time.Now()
		top10, searchErr := workspace.Retriever.Search(ctx, retrieval.SearchRequest{
			SnapshotID:       caseID,
			CodeIndexBuildID: workspace.CodeIndexBuildID,
			RetrievalBuildID: workspace.RetrievalBuildID,
			Query:            query,
			TopK:             10,
		})
		status.LatencyMs = time.Since(searchStarted).Milliseconds()
		workspace.Close()
		if searchErr != nil {
			status.Status = "PRODUCT_FAILURE"
			status.ErrorClass = "REPOLENS_PRODUCT"
			status.Error = searchErr.Error()
			result.Cases = append(result.Cases, status)
			_ = writeJSON(filepath.Join(caseDir, "status.json"), status)
			continue
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
			E2EStatus:         e2eStatus,
		}
		// Persist the prediction before loading evaluator-only Ground Truth.
		if err := writeJSON(filepath.Join(caseDir, "prediction.json"), prediction); err != nil {
			return nil, fmt.Errorf("write %s prediction: %w", caseID, err)
		}
		if err := writeJSON(filepath.Join(caseDir, "retrieval_top10.json"), top10); err != nil {
			return nil, fmt.Errorf("write %s retrieval results: %w", caseID, err)
		}

		truth, truthErr := r.Dataset.LoadGroundTruth(caseID)
		if truthErr != nil {
			status.Status = "PRODUCT_FAILURE"
			status.ErrorClass = "REPOLENS_PRODUCT"
			status.Error = truthErr.Error()
			result.Cases = append(result.Cases, status)
			_ = writeJSON(filepath.Join(caseDir, "status.json"), status)
			continue
		}
		status.HitAt5, status.HitAt10, status.ReciprocalRank = retrievalMetrics(top10, truth.PrimaryFiles)
		status.Status = "RETRIEVAL_COMPLETED_E2E_NOT_RUN"
		result.Cases = append(result.Cases, status)
		if err := writeJSON(filepath.Join(caseDir, "status.json"), status); err != nil {
			return nil, fmt.Errorf("write %s status: %w", caseID, err)
		}
	}

	result.Metrics = aggregateMetrics(result.Cases)
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
	Retriever        *retrieval.ProductionRetriever
	CodeIndexBuildID int64
	RetrievalBuildID int64
	db               *gorm.DB
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
		return nil, err
	}

	dbPath := filepath.Join(artifactDir, "state.sqlite")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open benchmark state database: %w", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		return nil, fmt.Errorf("migrate benchmark state database: %w", err)
	}
	ciStore := codeintelstore.NewStore(db)
	buildContext := codeintel.DefaultBuildContext()
	analysis, err := codeintel.NewAnalyzer().Analyze(ctx, sourceDir, buildContext)
	if err != nil {
		return nil, fmt.Errorf("CodeIndex analysis failed: %w", err)
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
		return nil, fmt.Errorf("create CodeIndexBuild: %w", err)
	}
	if err := ciStore.SaveAnalysisResult(ctx, build.ID, analysis); err != nil {
		return nil, fmt.Errorf("save CodeIndex: %w", err)
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
		return nil, fmt.Errorf("create RetrievalBuild: %w", err)
	}
	if err := ciStore.MarkRetrievalBuilding(ctx, retrievalBuild.ID); err != nil {
		return nil, fmt.Errorf("mark RetrievalBuild building: %w", err)
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
		return nil, fmt.Errorf("publish Retrieval artifact: %w", err)
	}
	if err := ciStore.CompleteRetrievalBuild(ctx, retrievalBuild.ID, artifactPath, artifactHash, idx.TotalDocs); err != nil {
		return nil, fmt.Errorf("finalize RetrievalBuild: %w", err)
	}
	return &productionWorkspace{
		Retriever:        retrieval.NewProductionRetriever(ciStore, indexRoot),
		CodeIndexBuildID: build.ID,
		RetrievalBuildID: retrievalBuild.ID,
		db:               db,
	}, nil
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
	metrics := Metrics{TotalCases: len(cases), CitationStatus: "NOT_RUN_RETRIEVAL_ONLY", RootCauseStatus: "NOT_RUN_PROVIDER_NOT_CONFIGURED"}
	for _, status := range cases {
		switch status.Status {
		case "RETRIEVAL_COMPLETED_E2E_NOT_RUN":
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
