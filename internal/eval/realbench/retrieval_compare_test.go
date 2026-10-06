package realbench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/retrieval"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/structural"
)

type countingComparisonFetcher struct{ calls int }

func (f *countingComparisonFetcher) Fetch(ctx context.Context, input Input, dir string) error {
	f.calls++
	return (syntheticFetcher{}).Fetch(ctx, input, dir)
}

func TestPairedRetrievalUsesProductionBuildsAndArtifacts(t *testing.T) {
	for _, candidate := range []string{codeintelmodel.StrategyBM25Structural, codeintelmodel.StrategyBM25StructuralV2} {
		t.Run(candidate, func(t *testing.T) { testPairedRetrievalUsesProductionBuildsAndArtifacts(t, candidate) })
	}
}

func testPairedRetrievalUsesProductionBuildsAndArtifacts(t *testing.T, candidate string) {
	dataset, err := LoadInputs(writeSyntheticDataset(t))
	if err != nil {
		t.Fatal(err)
	}
	fetcher := &countingComparisonFetcher{}
	runner := NewRunner(dataset)
	runner.Fetcher = fetcher
	// Provider configuration must have no bearing on this path.
	t.Setenv("REPOLENS_REALBENCH_PROVIDER", "openai")
	t.Setenv("REPOLENS_REALBENCH_API_KEY", "test-no-network")
	t.Setenv("REPOLENS_REALBENCH_MODEL", "test-model")
	t.Setenv("REPOLENS_REALBENCH_BASE_URL", "http://127.0.0.1:1")
	result, err := runner.CompareRetrieval(context.Background(), RunOptions{CacheDir: t.TempDir(), ArtifactRoot: t.TempDir(), CandidateStrategy: candidate})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 1 || result.Metrics.CompletedCases != 1 || result.Metrics.ProductFailures != 0 || result.Metrics.InfraErrors != 0 {
		t.Fatalf("unexpected paired run: %+v, fetches %d", result.Metrics, fetcher.calls)
	}
	c := result.Cases[0]
	a, b := c.BM25.Build, c.Candidate.Build
	if a.ID == b.ID || a.Strategy != codeintelmodel.StrategyBM25 || b.Strategy != candidate || a.ConfigHash == b.ConfigHash {
		t.Fatalf("build isolation violated: %+v %+v", a, b)
	}
	if a.CodeIndexBuildID != b.CodeIndexBuildID || a.CodeIndexBuildID != c.CodeIndexBuild.ID || a.DocumentCount != b.DocumentCount || a.RetrievalVersion != b.RetrievalVersion || a.TokenizerVersion != b.TokenizerVersion || a.ArtifactHash != b.ArtifactHash {
		t.Fatalf("paired inputs differ: %+v %+v", a, b)
	}
	for i, search := range []*ComparisonSearch{c.BM25, c.Candidate} {
		build := search.Build
		if search.Status != "COMPLETED" {
			t.Fatalf("wrong search status: %s", search.Status)
		}
		if build.ConfigHash != codeintelmodel.RetrievalConfigHash(build.Strategy) || build.Status != codeintelmodel.BuildStatusReady {
			t.Fatalf("wrong build identity: %+v", build)
		}
		idx, err := artifact.LoadIndexVerified(build.ArtifactPath, build.ID, build.ArtifactHash, build.Strategy)
		if err != nil {
			t.Fatal(err)
		}
		if idx.TotalDocs != build.DocumentCount || idx.TotalDocs == 0 {
			t.Fatal("document counts disagree")
		}
		bodyFound := false
		for _, doc := range idx.Documents {
			if doc.SymbolKeyHash == "" || doc.SymbolName == "" {
				t.Fatalf("missing Symbol identity: %+v", doc)
			}
			if strings.Contains(doc.Content, "return input") {
				bodyFound = true
			}
		}
		if !bodyFound {
			t.Fatal("RealBench artifact lacks production source body")
		}
		if len(search.Top8) == 0 || len(search.Top8) > 8 {
			t.Fatalf("unexpected Top8: %+v", search.Top8)
		}
		source := "symbol_bm25"
		if i == 1 {
			source = "symbol_bm25_structural"
			if candidate == codeintelmodel.StrategyBM25StructuralV2 {
				source = "symbol_bm25_structural_v2"
			}
		}
		for _, hit := range search.Top8 {
			if hit.RetrievalSource != source {
				t.Fatalf("search bypassed production strategy: %+v", hit)
			}
		}
	}
	if result.CandidateStrategy != candidate || c.CandidateStrategy != candidate || c.CandidateRetrievalBuild.ID != b.ID || result.EvidenceScope != "DEV / DIAGNOSTIC EVIDENCE ONLY" {
		t.Fatal("missing explicit candidate metadata")
	}
	if candidate == codeintelmodel.StrategyBM25StructuralV2 {
		if c.Structural != nil || result.Metrics.Structural != nil || c.Candidate.Expansion == nil {
			t.Fatal("V2 mislabeled as legacy V1 or missing trace")
		}
		report, _ := os.ReadFile(filepath.Join(result.RunDir, "retrieval_compare_report.md"))
		if !strings.Contains(string(report), "BM25_STRUCTURAL_V2") || !strings.Contains(string(report), "DEV / DIAGNOSTIC EVIDENCE ONLY") || !strings.Contains(string(report), "Expansion trace") {
			t.Fatal("candidate report metadata missing")
		}
	}
	if c.Query != retrieval.BuildQuery(dataset.Inputs[0].Input.IssueTitle, dataset.Inputs[0].Input.IssueDescription, dataset.Inputs[0].Input.ErrorLog) || c.TopK != 8 {
		t.Fatal("initial retrieval contract drifted")
	}
	prediction, err := os.ReadFile(filepath.Join(result.RunDir, "cases", c.CaseID, "retrieval_compare_prediction.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, leaked := range []string{"DO_NOT_LEAK_GROUND_TRUTH", "primary_files", "supporting_files", "recall_delta", `"metrics"`} {
		if strings.Contains(string(prediction), leaked) {
			t.Fatalf("prediction contains label data %q", leaked)
		}
	}
	for _, name := range []string{"retrieval_compare.json", "retrieval_compare_metrics.json", "retrieval_compare_report.md"} {
		if _, err := os.Stat(filepath.Join(result.RunDir, name)); err != nil {
			t.Fatal(err)
		}
	}
	var persisted RetrievalComparison
	readTestJSON(t, filepath.Join(result.RunDir, "retrieval_compare.json"), &persisted)
	if persisted.Metrics.CompletedCases != 1 || persisted.Cases[0].BM25.Metrics == nil {
		t.Fatal("paired report was not persisted")
	}
	// Rebuilding from persisted symbols through the shared builder produces the
	// exact same bytes as RealBench's published artifact.
	workspaceDir := t.TempDir()
	w, err := prepareProductionWorkspace(context.Background(), dataset.Inputs[0].Input, "other-snapshot", t.TempDir(), workspaceDir, syntheticFetcher{}, codeintelmodel.StrategyBM25, codeintelmodel.StrategyBM25Structural)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	symbols, err := w.CodeIndexStore.ListAllSymbols(context.Background(), w.CodeIndexBuildID)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := retrieval.BuildSymbolIndex(context.Background(), symbols, &retrieval.SymbolIndexSource{Store: w.SnapshotStore, RepositoryID: "REAL-999", SnapshotID: "other-snapshot"})
	if err != nil {
		t.Fatal(err)
	}
	var expected bytes.Buffer
	if err := idx.Save(&expected); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(filepath.Join(w.RetrievalBuilds[codeintelmodel.StrategyBM25].ArtifactPath, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(expected.Bytes(), actual) {
		t.Fatal("RealBench and shared production builder differ")
	}
}

func TestComparisonQueryAndTopK(t *testing.T) {
	input := Input{IssueTitle: "handler fails", IssueDescription: strings.Repeat("irrelevant prose ", 400), ErrorLog: "panic: Handle\nexample.com/pkg.Handle /src/handler.go:12"}
	spy := &retrieverSpy{}
	query, _, err := searchComparisonInput(context.Background(), spy, input, "snapshot", 11, 12)
	if err != nil {
		t.Fatal(err)
	}
	if query != retrieval.BuildQuery(input.IssueTitle, input.IssueDescription, input.ErrorLog) || spy.request.Query != query || spy.request.TopK != 8 || spy.request.SnapshotID != "snapshot" || spy.request.CodeIndexBuildID != 11 || spy.request.RetrievalBuildID != 12 {
		t.Fatalf("wrong request: %+v", spy.request)
	}
}

func TestV2PredictionsPersistBeforeAnyGroundTruthParsing(t *testing.T) {
	dataset, err := LoadInputs(writeSyntheticDataset(t))
	if err != nil {
		t.Fatal(err)
	}
	// Malformed labels would make a pre-search digest verification fail. Input
	// loading and both production predictions must still complete first.
	if err := os.WriteFile(filepath.Join(dataset.Inputs[0].CaseDir, "ground_truth.json"), []byte("invalid evaluator-only labels"), 0644); err != nil {
		t.Fatal(err)
	}
	fetcher := &countingComparisonFetcher{}
	runner := NewRunner(dataset)
	runner.Fetcher = fetcher
	artifacts := t.TempDir()
	_, err = runner.CompareRetrieval(context.Background(), RunOptions{CacheDir: t.TempDir(), ArtifactRoot: artifacts, CandidateStrategy: codeintelmodel.StrategyBM25StructuralV2})
	if err == nil || fetcher.calls != 1 {
		t.Fatal("labels parsed before production prediction or invalid labels accepted")
	}
	paths, err := filepath.Glob(filepath.Join(artifacts, "*", "cases", "REAL-999", "retrieval_compare_prediction.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal("prediction was not saved before loading labels")
	}
	var prediction RetrievalComparisonCase
	readTestJSON(t, paths[0], &prediction)
	if prediction.BM25.Status != "COMPLETED" || prediction.Candidate.Status != "COMPLETED" || prediction.CandidateStrategy != codeintelmodel.StrategyBM25StructuralV2 || prediction.Candidate.Metrics != nil || len(prediction.PrimaryFiles) != 0 {
		t.Fatal("prediction contains scoring or did not use V2")
	}
}

func TestComparisonCandidateSelection(t *testing.T) {
	for _, tc := range []struct {
		input, want string
		valid       bool
	}{
		{"", codeintelmodel.StrategyBM25Structural, true}, {codeintelmodel.StrategyBM25Structural, codeintelmodel.StrategyBM25Structural, true}, {codeintelmodel.StrategyBM25StructuralV2, codeintelmodel.StrategyBM25StructuralV2, true}, {codeintelmodel.StrategyBM25, "", false}, {"UNKNOWN", "", false},
	} {
		got, err := ComparisonCandidateStrategy(tc.input)
		if (err == nil) != tc.valid || got != tc.want {
			t.Fatalf("candidate=%q got=%q err=%v", tc.input, got, err)
		}
	}
	dataset, err := LoadInputs(writeSyntheticDataset(t))
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(dataset)
	fetcher := &countingComparisonFetcher{}
	runner.Fetcher = fetcher
	_, err = runner.CompareRetrieval(context.Background(), RunOptions{CandidateStrategy: "UNKNOWN", ArtifactRoot: t.TempDir()})
	if err == nil || fetcher.calls != 0 {
		t.Fatal("invalid strategy launched benchmark")
	}
}

func TestComparisonQuality(t *testing.T) {
	for _, tc := range []struct {
		name        string
		paths, gold []string
		recall, rr  float64
	}{
		{"duplicate symbols and normalized gold", []string{`DIR\A.go`, "dir/a.go", "other.go"}, []string{"dir/a.go", "DIR/A.GO", "b.go"}, .5, 1},
		{"rank three", []string{"a.go", "b.go", "c.go"}, []string{"C.GO"}, 1, 1.0 / 3},
		{"all primary", []string{"a.go", "b.go"}, []string{"a.go", "b.go"}, 1, 1},
		{"no hit", []string{"other.go"}, []string{"gold.go"}, 0, 0},
		{"empty results", nil, []string{"gold.go"}, 0, 0},
		{"outside top eight", []string{"x", "x", "x", "x", "x", "x", "x", "x", "gold"}, []string{"gold"}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var results []retrieval.SearchResult
			for _, path := range tc.paths {
				results = append(results, retrieval.SearchResult{Path: path})
			}
			got := comparisonQuality(results, tc.gold)
			if got.RecallAt8 != tc.recall || got.RR != tc.rr {
				t.Fatalf("quality=%+v, want %v %v", got, tc.recall, tc.rr)
			}
		})
	}
}

type failedComparisonFetcher struct{ product bool }

func (f failedComparisonFetcher) Fetch(ctx context.Context, input Input, dir string) error {
	if !f.product {
		return errors.New("network unavailable")
	}
	return os.MkdirAll(dir, 0755) // Empty checkout makes CodeIndex fail.
}

func TestComparisonPersistsFailuresWithoutFakeAggregate(t *testing.T) {
	for _, product := range []bool{false, true} {
		dataset, err := LoadInputs(writeSyntheticDataset(t))
		if err != nil {
			t.Fatal(err)
		}
		runner := NewRunner(dataset)
		runner.Fetcher = failedComparisonFetcher{product: product}
		result, err := runner.CompareRetrieval(context.Background(), RunOptions{CacheDir: t.TempDir(), ArtifactRoot: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		if result.Metrics.CompletedCases != 0 || result.Metrics.BM25 != nil || result.Metrics.Structural != nil || result.Cases[0].Error == "" {
			t.Fatalf("failure became fake result: %+v", result)
		}
		if product && result.Metrics.ProductFailures != 1 || !product && result.Metrics.InfraErrors != 1 {
			t.Fatalf("misclassified failure: %+v", result.Metrics)
		}
		if _, err := os.Stat(filepath.Join(result.RunDir, "retrieval_compare_report.md")); err != nil {
			t.Fatal(err)
		}
	}
}

func TestComparisonAggregationAndPercentiles(t *testing.T) {
	if got := comparisonPercentile([]float64{1, 3}, .95); math.Abs(got-2.9) > 1e-12 {
		t.Fatalf("p95=%v", got)
	}
	if got := comparisonPercentile([]float64{.01}, .5); got != .01 {
		t.Fatalf("sub-ms precision lost: %v", got)
	}
	cases := []RetrievalComparisonCase{{CaseID: "ok", Status: "COMPLETED", Outcome: "better", BM25: &ComparisonSearch{Metrics: &ComparisonQuality{RecallAt8: .5, RR: .25}, LatencyMS: .1}, Structural: &ComparisonSearch{Metrics: &ComparisonQuality{RecallAt8: 1, RR: .5}, LatencyMS: .2}}, {CaseID: "failed", ErrorClass: string(failureExternalInfra)}}
	m := aggregateComparison(cases)
	if m.CompletedCases != 1 || m.TotalCases != 2 || m.BM25.MeanRecallAt8 != .5 || m.Structural.MRR != .5 || m.InfraErrors != 1 || len(m.BetterCases) != 1 {
		t.Fatalf("wrong paired denominator: %+v", m)
	}
}

func devDecisionFixture() *RetrievalComparison {
	r := &RetrievalComparison{CandidateStrategy: codeintelmodel.StrategyBM25StructuralV2, DatasetVersion: "realbench-v2"}
	for i := 0; i < 10; i++ {
		a := ComparisonQuality{RecallAt8: 1, RR: 1}
		b := a
		if i == 0 {
			a.RecallAt8 = 0
			a.RR = 0
		}
		if i == 1 {
			a.RecallAt8 = .5
			b.RecallAt8 = .5
		}
		trace := &structural.ExpansionSearchTrace{Depth: 1}
		base := []retrieval.SearchResult{{Path: "gold.go"}}
		candidate := base
		if i == 0 {
			base = []retrieval.SearchResult{{Path: "miss.go"}}
			trace.SeedCount = 1
			trace.RelationQueries = 2
			trace.ExpandedCandidates = 1
			trace.Candidates = []structural.ExpansionResultTrace{{Expanded: true, SymbolKeyHash: "new", BestSeed: &structural.ExpansionReason{SeedSymbolKeyHash: "seed", SeedBM25Rank: 1}, EffectiveRank: 2, FinalRank: 3}}
		}
		r.Cases = append(r.Cases, RetrievalComparisonCase{CaseID: fmt.Sprintf("test-%d", i), CandidateStrategy: r.CandidateStrategy, Status: "COMPLETED", PrimaryFiles: []string{"gold.go"}, BM25: &ComparisonSearch{Metrics: &a, Top8: base}, Candidate: &ComparisonSearch{Metrics: &b, Top8: candidate, Expansion: trace}})
	}
	r.Metrics = aggregateComparison(r.Cases)
	return r
}

func TestV2DevDecisionIsStrictAndNeverPromotion(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*RetrievalComparison)
		want string
	}{
		{"go", func(r *RetrievalComparison) {}, "V2_DEV_GO"},
		{"no gain", func(r *RetrievalComparison) { r.Metrics.Candidate.MeanRecallAt8 = .85 }, "STOP_STRUCTURAL"},
		{"recall regression", func(r *RetrievalComparison) { r.Cases[2].Candidate.Metrics.RecallAt8 = .5 }, "STOP_STRUCTURAL"},
		{"mrr alone is not gate", func(r *RetrievalComparison) { r.Cases[2].Candidate.Metrics.RR = .25 }, "V2_DEV_GO"},
		{"no newly retrieved gold", func(r *RetrievalComparison) { r.Cases[0].Candidate.Top8 = r.Cases[0].BM25.Top8 }, "STOP_STRUCTURAL"},
		{"product failure", func(r *RetrievalComparison) { r.Metrics.ProductFailures = 1 }, "STOP_STRUCTURAL"},
		{"deep traversal", func(r *RetrievalComparison) { r.Cases[0].Candidate.Expansion.Depth = 2 }, "STOP_STRUCTURAL"},
		{"too many seeds", func(r *RetrievalComparison) { r.Cases[0].Candidate.Expansion.SeedCount = 9 }, "STOP_STRUCTURAL"},
		{"total fanout", func(r *RetrievalComparison) { r.Cases[0].Candidate.Expansion.ExpandedCandidates = 5 }, "STOP_STRUCTURAL"},
		{"per seed fanout", func(r *RetrievalComparison) {
			tr := r.Cases[0].Candidate.Expansion
			tr.Candidates = append(tr.Candidates, tr.Candidates[0])
		}, "STOP_STRUCTURAL"},
		{"too many queries", func(r *RetrievalComparison) { r.Cases[0].Candidate.Expansion.RelationQueries = 17 }, "STOP_STRUCTURAL"},
		{"too many rows", func(r *RetrievalComparison) { r.Cases[0].Candidate.Expansion.RelationsExamined = 129 }, "STOP_STRUCTURAL"},
		{"single case", func(r *RetrievalComparison) { r.Metrics.TotalCases = 1 }, ""},
		{"infra incomplete", func(r *RetrievalComparison) { r.Metrics.InfraErrors = 1 }, ""},
		{"legacy v1", func(r *RetrievalComparison) { r.CandidateStrategy = codeintelmodel.StrategyBM25Structural }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := devDecisionFixture()
			tc.edit(r)
			if got := v2DevDecision(r); got != tc.want {
				t.Fatalf("decision=%s want=%s", got, tc.want)
			}
		})
	}
}
