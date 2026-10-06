package realbench

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/retrieval"
	"repolens/internal/retrieval/artifact"
)

type countingComparisonFetcher struct{ calls int }

func (f *countingComparisonFetcher) Fetch(ctx context.Context, input Input, dir string) error {
	f.calls++
	return (syntheticFetcher{}).Fetch(ctx, input, dir)
}

func TestPairedRetrievalUsesProductionBuildsAndArtifacts(t *testing.T) {
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
	result, err := runner.CompareRetrieval(context.Background(), RunOptions{CacheDir: t.TempDir(), ArtifactRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if fetcher.calls != 1 || result.Metrics.CompletedCases != 1 || result.Metrics.ProductFailures != 0 || result.Metrics.InfraErrors != 0 {
		t.Fatalf("unexpected paired run: %+v, fetches %d", result.Metrics, fetcher.calls)
	}
	c := result.Cases[0]
	a, b := c.BM25.Build, c.Structural.Build
	if a.ID == b.ID || a.Strategy != codeintelmodel.StrategyBM25 || b.Strategy != codeintelmodel.StrategyBM25Structural || a.ConfigHash == b.ConfigHash {
		t.Fatalf("build isolation violated: %+v %+v", a, b)
	}
	if a.CodeIndexBuildID != b.CodeIndexBuildID || a.CodeIndexBuildID != c.CodeIndexBuild.ID || a.DocumentCount != b.DocumentCount || a.RetrievalVersion != b.RetrievalVersion || a.TokenizerVersion != b.TokenizerVersion || a.ArtifactHash != b.ArtifactHash {
		t.Fatalf("paired inputs differ: %+v %+v", a, b)
	}
	for i, search := range []*ComparisonSearch{c.BM25, c.Structural} {
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
		}
		for _, hit := range search.Top8 {
			if hit.RetrievalSource != source {
				t.Fatalf("search bypassed production strategy: %+v", hit)
			}
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
