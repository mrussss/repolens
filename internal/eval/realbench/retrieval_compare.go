package realbench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/retrieval"
)

const comparisonTopK = 8

type RetrievalComparison struct {
	RunDir              string                     `json:"run_dir"`
	DatasetVersion      string                     `json:"dataset_version"`
	DatasetManifestHash string                     `json:"dataset_manifest_hash"`
	RepoLensGitCommit   string                     `json:"repolens_git_commit"`
	WorkingTreeClean    bool                       `json:"working_tree_clean"`
	Timestamp           time.Time                  `json:"timestamp"`
	GoVersion           string                     `json:"go_version"`
	OS                  string                     `json:"os"`
	Arch                string                     `json:"arch"`
	Cases               []RetrievalComparisonCase  `json:"cases"`
	Metrics             RetrievalComparisonMetrics `json:"metrics"`
}

type RetrievalComparisonCase struct {
	CaseID              string                         `json:"case_id"`
	Repository          Repository                     `json:"repository"`
	BuggyCommitSHA      string                         `json:"buggy_commit_sha"`
	SnapshotID          string                         `json:"snapshot_id"`
	DatasetVersion      string                         `json:"dataset_version"`
	DatasetManifestHash string                         `json:"dataset_manifest_hash"`
	CodeIndexBuild      *codeintelmodel.CodeIndexBuild `json:"code_index_build,omitempty"`
	Query               string                         `json:"query"`
	TopK                int                            `json:"top_k"`
	SearchOrder         []string                       `json:"search_order"`
	BM25                *ComparisonSearch              `json:"bm25,omitempty"`
	Structural          *ComparisonSearch              `json:"bm25_structural,omitempty"`
	PrimaryFiles        []string                       `json:"primary_files,omitempty"`
	SupportingFiles     []string                       `json:"supporting_files,omitempty"`
	RecallDelta         *float64                       `json:"recall_delta,omitempty"`
	RRDelta             *float64                       `json:"rr_delta,omitempty"`
	Outcome             string                         `json:"outcome,omitempty"`
	Status              string                         `json:"status"`
	ErrorClass          string                         `json:"error_class,omitempty"`
	Error               string                         `json:"error,omitempty"`
}

type ComparisonSearch struct {
	Build     *codeintelmodel.RetrievalBuild `json:"retrieval_build"`
	Status    string                         `json:"status"`
	LatencyMS float64                        `json:"latency_ms"`
	Top8      []retrieval.SearchResult       `json:"top8"`
	Metrics   *ComparisonQuality             `json:"metrics,omitempty"`
}

type ComparisonQuality struct {
	RecallAt8 float64 `json:"recall_at_8"`
	RR        float64 `json:"rr"`
}

type ComparisonAggregate struct {
	MeanRecallAt8 float64 `json:"mean_recall_at_8"`
	MRR           float64 `json:"mrr"`
	LatencyP50MS  float64 `json:"latency_p50_ms"`
	LatencyP95MS  float64 `json:"latency_p95_ms"`
}

type RetrievalComparisonMetrics struct {
	TotalCases      int                  `json:"total_cases"`
	CompletedCases  int                  `json:"completed_cases"`
	InfraErrors     int                  `json:"infra_errors"`
	ProductFailures int                  `json:"product_failures"`
	BM25            *ComparisonAggregate `json:"bm25,omitempty"`
	Structural      *ComparisonAggregate `json:"bm25_structural,omitempty"`
	BetterCases     []string             `json:"better_cases"`
	EqualCases      []string             `json:"equal_cases"`
	WorseCases      []string             `json:"worse_cases"`
}

// CompareRetrieval measures the production initial retrieval contract without
// invoking a provider or agent. Ground truth is loaded only after predictions.
func (r *Runner) CompareRetrieval(ctx context.Context, opts RunOptions) (*RetrievalComparison, error) {
	if r == nil || r.Dataset == nil {
		return nil, errors.New("realbench dataset is required")
	}
	if opts.RunE2E {
		return nil, errors.New("paired retrieval does not run E2E")
	}
	hash, err := ComputeManifestHash(r.Dataset.Root, r.Dataset.Manifest.Cases)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(hash, r.Dataset.Manifest.ManifestHash) {
		return nil, errors.New("dataset manifest hash mismatch")
	}
	inputs := r.Dataset.Inputs
	if len(opts.CaseIDs) > 0 {
		inputs = nil
		seen := map[string]bool{}
		for _, id := range opts.CaseIDs {
			if seen[id] {
				return nil, fmt.Errorf("duplicate requested case %s", id)
			}
			input, ok := r.Dataset.Input(id)
			if !ok {
				return nil, fmt.Errorf("case %s is not in dataset", id)
			}
			seen[id] = true
			inputs = append(inputs, input)
		}
	}
	if opts.CacheDir == "" {
		opts.CacheDir = filepath.Join(".cache", "realbench")
	}
	if opts.ArtifactRoot == "" {
		opts.ArtifactRoot = filepath.Join("artifacts", "realbench")
	}
	runID := time.Now().UTC().Format("20060102T150405Z") + "-" + uuid.New().String()[:8]
	result := &RetrievalComparison{
		RunDir:         filepath.Join(opts.ArtifactRoot, runID),
		DatasetVersion: r.Dataset.Manifest.DatasetVersion, DatasetManifestHash: hash,
		RepoLensGitCommit: currentGitCommit(), WorkingTreeClean: workingTreeClean(),
		Timestamp: time.Now().UTC(), GoVersion: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		Cases: make([]RetrievalComparisonCase, 0, len(inputs)),
	}
	if err := os.MkdirAll(result.RunDir, 0755); err != nil {
		return nil, err
	}
	fetcher := r.Fetcher
	if fetcher == nil {
		fetcher = gitSnapshotFetcher{}
	}
	for i, input := range inputs {
		c := RetrievalComparisonCase{
			CaseID: input.Input.CaseID, Repository: input.Input.Repository, BuggyCommitSHA: input.Input.BuggyCommitSHA,
			SnapshotID: input.Input.CaseID, DatasetVersion: result.DatasetVersion, DatasetManifestHash: hash,
			Query: retrieval.BuildQuery(input.Input.IssueTitle, input.Input.IssueDescription, input.Input.ErrorLog), TopK: comparisonTopK,
		}
		caseDir := filepath.Join(result.RunDir, "cases", c.CaseID)
		err := os.MkdirAll(caseDir, 0755)
		if err == nil {
			err = r.compareCase(ctx, input, &c, opts.CacheDir, caseDir, fetcher, i%2 == 1)
		}
		if err != nil {
			c.Status, c.ErrorClass = classifyFailure(err)
			c.Error = err.Error()
		} else {
			c.Status = "COMPLETED"
		}
		result.Cases = append(result.Cases, c)
	}
	result.Metrics = aggregateComparison(result.Cases)
	if err := writeJSON(filepath.Join(result.RunDir, "retrieval_compare.json"), result); err != nil {
		return result, err
	}
	if err := writeJSON(filepath.Join(result.RunDir, "retrieval_compare_metrics.json"), result.Metrics); err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(result.RunDir, "retrieval_compare_report.md"), []byte(comparisonReport(result)), 0644); err != nil {
		return result, err
	}
	return result, nil
}

func (r *Runner) compareCase(ctx context.Context, input InputCase, c *RetrievalComparisonCase, cacheDir, caseDir string, fetcher SnapshotFetcher, structuralFirst bool) error {
	w, err := prepareProductionWorkspace(ctx, input.Input, c.SnapshotID, filepath.Join(cacheDir, c.CaseID), caseDir, fetcher,
		codeintelmodel.StrategyBM25, codeintelmodel.StrategyBM25Structural)
	if err != nil {
		return err
	}
	defer w.Close()
	c.CodeIndexBuild = w.CodeIndexBuild
	c.BM25 = &ComparisonSearch{Build: w.RetrievalBuilds[codeintelmodel.StrategyBM25], Status: "NOT_RUN"}
	c.Structural = &ComparisonSearch{Build: w.RetrievalBuilds[codeintelmodel.StrategyBM25Structural], Status: "NOT_RUN"}
	searches := []*ComparisonSearch{c.BM25, c.Structural}
	if structuralFirst {
		searches[0], searches[1] = searches[1], searches[0]
	}
	for _, s := range searches {
		c.SearchOrder = append(c.SearchOrder, s.Build.Strategy)
		started := time.Now()
		_, s.Top8, err = searchComparisonInput(ctx, w.Retriever, input.Input, c.SnapshotID, w.CodeIndexBuildID, s.Build.ID)
		s.LatencyMS = float64(time.Since(started).Nanoseconds()) / 1e6
		if err != nil {
			s.Status = "FAILED"
			return productFailure("production search "+s.Build.Strategy, err)
		}
		s.Status = "COMPLETED"
	}
	// No labels or scores derived from labels reach the retriever or this file.
	if err := writeJSON(filepath.Join(caseDir, "retrieval_compare_prediction.json"), c); err != nil {
		return productFailure("write paired prediction", err)
	}
	truth, err := r.Dataset.LoadGroundTruth(c.CaseID)
	if err != nil {
		return productFailure("load ground truth", err)
	}
	c.PrimaryFiles, c.SupportingFiles = truth.PrimaryFiles, truth.SupportingFiles
	a, b := comparisonQuality(c.BM25.Top8, truth.PrimaryFiles), comparisonQuality(c.Structural.Top8, truth.PrimaryFiles)
	c.BM25.Metrics, c.Structural.Metrics = &a, &b
	dRecall, dRR := b.RecallAt8-a.RecallAt8, b.RR-a.RR
	c.RecallDelta, c.RRDelta = &dRecall, &dRR
	c.Outcome = "equal"
	if dRecall > 0 || (dRecall == 0 && dRR > 0) {
		c.Outcome = "better"
	}
	if dRecall < 0 || (dRecall == 0 && dRR < 0) {
		c.Outcome = "worse"
	}
	return nil
}

func searchComparisonInput(ctx context.Context, retriever retrieval.Retriever, input Input, snapshotID string, codeIndexBuildID, retrievalBuildID int64) (string, []retrieval.SearchResult, error) {
	query := retrieval.BuildQuery(input.IssueTitle, input.IssueDescription, input.ErrorLog)
	results, err := retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: snapshotID, CodeIndexBuildID: codeIndexBuildID, RetrievalBuildID: retrievalBuildID,
		Query: query, TopK: comparisonTopK,
	})
	return query, results, err
}

func comparisonQuality(results []retrieval.SearchResult, primaryFiles []string) ComparisonQuality {
	if len(results) > comparisonTopK {
		results = results[:comparisonTopK]
	}
	_, _, rr := retrievalMetrics(results, primaryFiles)
	gold, found := map[string]bool{}, map[string]bool{}
	for _, path := range primaryFiles {
		gold[normalizePath(path)] = true
	}
	for _, result := range results {
		path := normalizePath(result.Path)
		if gold[path] {
			found[path] = true
		}
	}
	recall := 0.0
	if len(gold) > 0 {
		recall = float64(len(found)) / float64(len(gold))
	}
	return ComparisonQuality{RecallAt8: recall, RR: rr}
}

func aggregateComparison(cases []RetrievalComparisonCase) RetrievalComparisonMetrics {
	m := RetrievalComparisonMetrics{TotalCases: len(cases), BetterCases: []string{}, EqualCases: []string{}, WorseCases: []string{}}
	a, b := &ComparisonAggregate{}, &ComparisonAggregate{}
	var aLatency, bLatency []float64
	for _, c := range cases {
		if c.Status != "COMPLETED" {
			if c.ErrorClass == string(failureExternalInfra) {
				m.InfraErrors++
			} else {
				m.ProductFailures++
			}
			continue
		}
		m.CompletedCases++
		a.MeanRecallAt8 += c.BM25.Metrics.RecallAt8
		a.MRR += c.BM25.Metrics.RR
		b.MeanRecallAt8 += c.Structural.Metrics.RecallAt8
		b.MRR += c.Structural.Metrics.RR
		aLatency = append(aLatency, c.BM25.LatencyMS)
		bLatency = append(bLatency, c.Structural.LatencyMS)
		switch c.Outcome {
		case "better":
			m.BetterCases = append(m.BetterCases, c.CaseID)
		case "equal":
			m.EqualCases = append(m.EqualCases, c.CaseID)
		case "worse":
			m.WorseCases = append(m.WorseCases, c.CaseID)
		}
	}
	if m.CompletedCases > 0 {
		for _, s := range []*ComparisonAggregate{a, b} {
			s.MeanRecallAt8 /= float64(m.CompletedCases)
			s.MRR /= float64(m.CompletedCases)
		}
		a.LatencyP50MS, a.LatencyP95MS = comparisonPercentile(aLatency, .5), comparisonPercentile(aLatency, .95)
		b.LatencyP50MS, b.LatencyP95MS = comparisonPercentile(bLatency, .5), comparisonPercentile(bLatency, .95)
		m.BM25, m.Structural = a, b
	}
	return m
}

// Linear interpolation between ordered observations, including endpoints.
func comparisonPercentile(values []float64, p float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	x := p * float64(len(sorted)-1)
	i := int(x)
	if i == len(sorted)-1 {
		return sorted[i]
	}
	return sorted[i] + (sorted[i+1]-sorted[i])*(x-float64(i))
}

func comparisonReport(r *RetrievalComparison) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Paired production retrieval\n\nDataset: `%s`; manifest: `%s`\n\nRepoLens: `%s`; clean tree: %t; Go: %s; platform: %s/%s\n\nCompleted: %d/%d; external infra errors: %d; product failures: %d.\n\n", r.DatasetVersion, r.DatasetManifestHash, r.RepoLensGitCommit, r.WorkingTreeClean, r.GoVersion, r.OS, r.Arch, r.Metrics.CompletedCases, r.Metrics.TotalCases, r.Metrics.InfraErrors, r.Metrics.ProductFailures)
	out.WriteString("Metrics average completed pairs only. Gold = unique normalized PrimaryFiles; TopK = 8. Latencies are single cold artifact-loading searches, excluding build time; search order alternates by case. Percentiles use linear interpolation. This is observational, with no promotion gate.\n\n")
	if r.Metrics.CompletedCases > 0 {
		out.WriteString("| Strategy | Mean Recall@8 | MRR | p50 (ms) | p95 (ms) |\n|---|---:|---:|---:|---:|\n")
		for i, s := range []*ComparisonAggregate{r.Metrics.BM25, r.Metrics.Structural} {
			name := codeintelmodel.StrategyBM25
			if i == 1 {
				name = codeintelmodel.StrategyBM25Structural
			}
			fmt.Fprintf(&out, "| %s | %.6f | %.6f | %.6f | %.6f |\n", name, s.MeanRecallAt8, s.MRR, s.LatencyP50MS, s.LatencyP95MS)
		}
	}
	fmt.Fprintf(&out, "\nStructural better (%d): %s\n\nEqual (%d): %s\n\nWorse (%d): %s\n", len(r.Metrics.BetterCases), strings.Join(r.Metrics.BetterCases, ", "), len(r.Metrics.EqualCases), strings.Join(r.Metrics.EqualCases, ", "), len(r.Metrics.WorseCases), strings.Join(r.Metrics.WorseCases, ", "))
	for _, c := range r.Cases {
		fmt.Fprintf(&out, "\n## %s\n\nRepository: %s; buggy commit: `%s`; Snapshot: `%s`\n\nStatus: %s\n\nQuery: %s\n\nPrimary files: %s\n\nSupporting files (unscored): %s\n\n", c.CaseID, c.Repository.FullName, c.BuggyCommitSHA, c.SnapshotID, c.Status, markdownCell(c.Query), strings.Join(c.PrimaryFiles, ", "), strings.Join(c.SupportingFiles, ", "))
		if c.Error != "" {
			fmt.Fprintf(&out, "Error (%s): %s\n\n", c.ErrorClass, markdownCell(c.Error))
		}
		if c.CodeIndexBuild != nil {
			fmt.Fprintf(&out, "CodeIndexBuild: %d; analyzer: %s; context hash: `%s`; symbols: %d; semantic relations: %d\n\n", c.CodeIndexBuild.ID, c.CodeIndexBuild.AnalyzerVersion, c.CodeIndexBuild.BuildContextHash, c.CodeIndexBuild.SymbolCount, c.CodeIndexBuild.SemanticRelationCount)
		}
		for _, s := range []*ComparisonSearch{c.BM25, c.Structural} {
			if s == nil {
				continue
			}
			fmt.Fprintf(&out, "### %s\n\nRetrievalBuild: %d; config hash: `%s`; artifact hash: `%s`; search status: %s\n\n", s.Build.Strategy, s.Build.ID, s.Build.ConfigHash, s.Build.ArtifactHash, s.Status)
			if s.Status != "NOT_RUN" {
				fmt.Fprintf(&out, "Latency: %.6f ms\n\n", s.LatencyMS)
			}
			if s.Metrics != nil {
				fmt.Fprintf(&out, "Recall@8: %.6f; RR: %.6f\n\n", s.Metrics.RecallAt8, s.Metrics.RR)
			}
			out.WriteString("| Rank | Path | Symbol | Score | Reason |\n|---:|---|---|---:|---|\n")
			for rank, hit := range s.Top8 {
				fmt.Fprintf(&out, "| %d | %s | %s | %.6f | %s |\n", rank+1, markdownCell(hit.Path), markdownCell(hit.Symbol), hit.Score, markdownCell(hit.RetrievalReason))
			}
		}
		if c.RecallDelta != nil {
			fmt.Fprintf(&out, "\nDelta (Structural − BM25): Recall@8 %+.6f; RR %+.6f; outcome: %s\n", *c.RecallDelta, *c.RRDelta, c.Outcome)
		}
	}
	return out.String()
}

func markdownCell(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "|", "&#124;", "\r", "", "\n", "<br>").Replace(s)
}
