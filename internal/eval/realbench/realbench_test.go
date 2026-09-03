package realbench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/agent"
	"repolens/internal/diagnosis"
	"repolens/internal/llm"
	"repolens/internal/retrieval"
)

func TestSyntheticRunnerKeepsGroundTruthOutOfPrediction(t *testing.T) {
	root := writeSyntheticDataset(t)
	dataset, err := LoadInputs(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(root); err != nil {
		t.Fatal(err)
	}

	runner := NewRunner(dataset)
	runner.Fetcher = syntheticFetcher{}
	result, err := runner.Run(context.Background(), RunOptions{
		CaseIDs:      []string{"REAL-999"},
		CacheDir:     filepath.Join(t.TempDir(), "cache"),
		ArtifactRoot: filepath.Join(t.TempDir(), "artifacts"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metrics.CompletedCases != 1 || result.Metrics.InfraErrors != 0 || result.Metrics.ProductFailures != 0 {
		t.Fatalf("unexpected metrics: %+v", result.Metrics)
	}
	if result.Metadata.E2EStatus != e2eNotRequested || result.Cases[0].E2EStatus != e2eNotRequested {
		t.Fatalf("unexpected not-requested E2E state: metadata=%s case=%s", result.Metadata.E2EStatus, result.Cases[0].E2EStatus)
	}
	data, err := os.ReadFile(filepath.Join(result.RunDir, "cases", "REAL-999", "prediction.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "DO_NOT_LEAK_GROUND_TRUTH") {
		t.Fatal("prediction contains evaluator-only sentinel")
	}
}

func TestValidateRejectsGroundTruthSentinelInInput(t *testing.T) {
	root := writeSyntheticDataset(t)
	inputPath := filepath.Join(root, "REAL-999", "input.json")
	var input Input
	readTestJSON(t, inputPath, &input)
	input.IssueDescription = "DO_NOT_LEAK_GROUND_TRUTH"
	writeTestJSON(t, inputPath, input)
	if _, err := Validate(root); err == nil || !strings.Contains(err.Error(), "leakage") {
		t.Fatalf("expected leakage validation error, got %v", err)
	}
}

func TestGroundTruthSentinelDoesNotReachRetriever(t *testing.T) {
	truth := GroundTruth{ExpectedRootCause: "DO_NOT_LEAK_GROUND_TRUTH"}
	input := Input{
		IssueTitle: "request fails", IssueDescription: "the request returns an error", ErrorLog: "ERROR request failed",
	}
	if !strings.Contains(truth.ExpectedRootCause, "DO_NOT_LEAK_GROUND_TRUTH") {
		t.Fatal("test truth sentinel was not initialized")
	}
	spy := &retrieverSpy{}
	query, _, err := searchInput(context.Background(), spy, input, "snap-1", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if query != spy.request.Query || strings.Contains(spy.request.Query, "DO_NOT_LEAK_GROUND_TRUTH") {
		t.Fatalf("retriever received unexpected query: %q", spy.request.Query)
	}
}

func TestGroundTruthSentinelDoesNotReachProvider(t *testing.T) {
	truth := GroundTruth{ExpectedRootCause: "DO_NOT_LEAK_GROUND_TRUTH"}
	input := Input{
		CaseID: "REAL-999", IssueTitle: "request fails", IssueDescription: "the request returns an error", ErrorLog: "ERROR request failed",
	}
	if !strings.Contains(truth.ExpectedRootCause, "DO_NOT_LEAK_GROUND_TRUTH") {
		t.Fatal("test truth sentinel was not initialized")
	}
	provider := &providerSpy{}
	loop := agent.NewAgentLoop(provider, agent.NewToolRegistry(), nil, agent.DefaultGuardConfig())
	run := &diagnosis.DiagnosisRun{
		ID: "run-leakage", RepositoryID: input.CaseID, SnapshotID: input.CaseID,
		IssueTitle: input.IssueTitle, IssueDescription: input.IssueDescription, ErrorLog: input.ErrorLog,
	}
	if _, err := loop.Run(context.Background(), run, &diagnosis.DiagnosisAttempt{ID: "attempt-leakage"}); err != nil {
		t.Fatal(err)
	}
	for _, request := range provider.requests {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "DO_NOT_LEAK_GROUND_TRUTH") {
				t.Fatalf("provider received evaluator-only sentinel in message: %q", message.Content)
			}
		}
	}
}

func TestFailureClassificationKeepsExternalAndProductSeparate(t *testing.T) {
	if status, class := classifyFailure(externalFailure("git fetch", os.ErrNotExist)); status != "INFRA_ERROR" || class != "EXTERNAL_INFRA" {
		t.Fatalf("external failure classified as %s/%s", status, class)
	}
	if status, class := classifyFailure(productFailure("CodeIndex analysis", os.ErrInvalid)); status != "PRODUCT_FAILURE" || class != "REPOLENS_PRODUCT" {
		t.Fatalf("product failure classified as %s/%s", status, class)
	}
	if class := errorClassFor(externalFailure("provider Generate", os.ErrDeadlineExceeded)); class != "EXTERNAL_INFRA" {
		t.Fatalf("provider failure classified as %s", class)
	}
	provider := classifiedProvider{Provider: &errorProviderSpy{err: os.ErrDeadlineExceeded}}
	if _, err := provider.Generate(context.Background(), llm.GenerateRequest{}); err == nil || errorClassFor(err) != "EXTERNAL_INFRA" {
		t.Fatalf("provider wrapper did not preserve external classification: %v", err)
	}
}

func TestE2EStatusDistinguishesRequestedStates(t *testing.T) {
	tests := []struct {
		name       string
		requested  bool
		configured bool
		want       string
	}{
		{name: "not requested", want: e2eNotRequested},
		{name: "provider missing", requested: true, want: e2eNotRunProviderUnconfigured},
		{name: "provider available starts as failure until completed", requested: true, configured: true, want: e2eFailure},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := e2eStatusFor(test.requested, test.configured); got != test.want {
				t.Fatalf("e2e status = %s, want %s", got, test.want)
			}
		})
	}
}

type retrieverSpy struct {
	request retrieval.SearchRequest
}

func (s *retrieverSpy) Search(_ context.Context, request retrieval.SearchRequest) ([]retrieval.SearchResult, error) {
	s.request = request
	return nil, nil
}

type providerSpy struct {
	requests []llm.GenerateRequest
}

type errorProviderSpy struct {
	err error
}

func (s *errorProviderSpy) Generate(_ context.Context, _ llm.GenerateRequest) (llm.GenerateResponse, error) {
	return llm.GenerateResponse{}, s.err
}

func (s *providerSpy) Generate(_ context.Context, request llm.GenerateRequest) (llm.GenerateResponse, error) {
	s.requests = append(s.requests, request)
	return llm.GenerateResponse{
		Message: llm.Message{Role: llm.RoleAssistant, Content: `{"summary":"ok","root_cause":"input-only","findings":[],"recommended_checks":[],"confidence":0.1}`},
	}, nil
}

type syntheticFetcher struct{}

func (syntheticFetcher) Fetch(_ context.Context, _ Input, sourceDir string) error {
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte("module example.com/realbench-synthetic\n\ngo 1.22\n"), 0644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(sourceDir, "handler.go"), []byte("package synthetic\n\n// Handle processes an input.\nfunc Handle(input string) string { return input }\n"), 0644)
}

func writeSyntheticDataset(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "REAL-999")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	input := Input{
		CaseID: "REAL-999", DatasetVersion: "realbench-test-v1",
		Repository:     Repository{FullName: "example/synthetic", CloneURL: "https://github.com/example/synthetic.git"},
		BuggyCommitSHA: strings.Repeat("1", 40), IssueTitle: "handler result is unexpected",
		IssueDescription: "The handler result does not match the caller input.", ErrorLog: "Observed result mismatch in the synthetic fixture.",
	}
	truth := GroundTruth{
		CaseID: "REAL-999", FixCommitSHA: strings.Repeat("2", 40),
		ExpectedRootCause: "DO_NOT_LEAK_GROUND_TRUTH: synthetic evaluator root cause",
		PrimaryFiles:      []string{"handler.go"}, RelevantSymbols: []string{"Handle"},
		RelevantLineRanges: map[string]LineRange{"handler.go": {Start: 4, End: 4}},
		Provenance: Provenance{
			IssueURL:     "https://github.com/example/synthetic/issues/1",
			FixPRURL:     "https://github.com/example/synthetic/pull/2",
			FixCommitURL: "https://github.com/example/synthetic/commit/" + strings.Repeat("2", 40),
		},
		CurationNotes: "Synthetic fixture for offline runner coverage.",
	}
	writeTestJSON(t, filepath.Join(root, "input.json"), input)
	writeTestJSON(t, filepath.Join(root, "ground_truth.json"), truth)
	manifest := Manifest{DatasetVersion: "realbench-test-v1", Cases: []string{"REAL-999"}}
	writeTestJSON(t, filepath.Join(filepath.Dir(root), "manifest.json"), manifest)
	hash, err := ComputeManifestHash(filepath.Dir(root), manifest.Cases)
	if err != nil {
		t.Fatal(err)
	}
	manifest.ManifestHash = hash
	writeTestJSON(t, filepath.Join(filepath.Dir(root), "manifest.json"), manifest)
	return filepath.Dir(root)
}

func readTestJSON(t *testing.T, path string, target interface{}) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

func writeTestJSON(t *testing.T, path string, value interface{}) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}
