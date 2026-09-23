package realbench

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"repolens/internal/agent"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/llm"
	"repolens/internal/retrieval"
	"repolens/internal/trace"
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
	quality, err := os.ReadFile(filepath.Join(result.RunDir, "cases", "REAL-999", "analysis_quality.json"))
	if err != nil {
		t.Fatalf("analysis quality artifact missing: %v", err)
	}
	if !strings.Contains(string(quality), `"parse_rate"`) || !strings.Contains(string(quality), `"typecheck_rate"`) {
		t.Fatalf("analysis quality artifact missing rates: %s", quality)
	}
	summary, err := os.ReadFile(filepath.Join(result.RunDir, "analysis_quality_summary.csv"))
	if err != nil {
		t.Fatalf("analysis quality summary missing: %v", err)
	}
	if !strings.Contains(string(summary), "case_id") || !strings.Contains(string(summary), "REAL-999") {
		t.Fatalf("analysis quality summary missing case: %s", summary)
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

func TestE2EMetricsRecordTraceAndUnreportedUsage(t *testing.T) {
	collector := newTraceCollector()
	if err := collector.Create(context.Background(), &trace.AgentStep{AttemptID: "attempt", Seq: 1, StepType: trace.StepTypeThinking}); err != nil {
		t.Fatal(err)
	}
	if err := collector.Create(context.Background(), &trace.AgentStep{AttemptID: "attempt", Seq: 2, StepType: trace.StepTypeToolCall, ToolName: "search_code"}); err != nil {
		t.Fatal(err)
	}
	metrics := metricsFromExecution(nil, collector, 42)
	if metrics.AgentRounds != 1 || metrics.ToolCalls != 1 || len(metrics.ToolNames) != 1 || metrics.ToolNames[0] != "search_code" {
		t.Fatalf("unexpected trace metrics: %+v", metrics)
	}
	if metrics.CachedTokens != "NOT_REPORTED" || metrics.ReasoningTokens != "NOT_REPORTED" {
		t.Fatalf("missing usage must be explicit: %+v", metrics)
	}
}

func TestE2EMetricsRecordFinishReasonFromExecution(t *testing.T) {
	metrics := metricsFromExecution(&agent.ExecutionResult{
		FinishReason:     "length",
		PromptTokens:     10,
		CompletionTokens: 20,
		ReasoningTokens:  20,
	}, newTraceCollector(), 42)
	if metrics.FinishReason != "length" || metrics.OutputTokens != 20 || metrics.ReasoningTokens != 20 {
		t.Fatalf("truncation evidence was not preserved: %+v", metrics)
	}
}

func TestConfiguredRealBenchMaxOutputTokens(t *testing.T) {
	t.Setenv("REPOLENS_REALBENCH_MAX_OUTPUT_TOKENS", "")
	if got := configuredRealBenchMaxOutputTokens(); got != 0 {
		t.Fatalf("unset override = %d, want 0", got)
	}
	t.Setenv("REPOLENS_REALBENCH_MAX_OUTPUT_TOKENS", "4096")
	if got := configuredRealBenchMaxOutputTokens(); got != 4096 {
		t.Fatalf("configured override = %d, want 4096", got)
	}
	t.Setenv("REPOLENS_REALBENCH_MAX_OUTPUT_TOKENS", "invalid")
	if got := configuredRealBenchMaxOutputTokens(); got != 0 {
		t.Fatalf("invalid override = %d, want 0", got)
	}
}

func TestRealBenchGuardConfigUsesProductionDefaultAndOverride(t *testing.T) {
	t.Setenv("REPOLENS_MAX_OUTPUT_TOKENS", "")
	t.Setenv("REPOLENS_REALBENCH_MAX_OUTPUT_TOKENS", "")
	if got := realBenchGuardConfig().MaxOutputTokens; got != 4096 {
		t.Fatalf("unset override max output tokens = %d, want 4096", got)
	}
	t.Setenv("REPOLENS_REALBENCH_MAX_OUTPUT_TOKENS", "2048")
	if got := realBenchGuardConfig().MaxOutputTokens; got != 2048 {
		t.Fatalf("override max output tokens = %d, want 2048", got)
	}
}

func TestConfiguredRealBenchGenerationOptions(t *testing.T) {
	t.Setenv("REPOLENS_REALBENCH_REASONING_EFFORT", "")
	t.Setenv("REPOLENS_REALBENCH_RESPONSE_FORMAT", "")
	t.Setenv("REPOLENS_REASONING_EFFORT", "")
	defaultOptions := configuredRealBenchGenerationOptions()
	if defaultOptions.ReasoningEffort != "low" || defaultOptions.ResponseFormat != "json_object" || defaultOptions.MetadataReasoningEffort() != "low" {
		t.Fatalf("unset generation options changed the default: %+v", defaultOptions)
	}
	if got := defaultOptions.AgentOptions(); got.ReasoningEffort != "low" || got.ResponseFormat == nil || got.ResponseFormat.Type != "json_object" {
		t.Fatalf("unset generation options changed the request: %+v", got)
	}

	t.Setenv("REPOLENS_REALBENCH_REASONING_EFFORT", "low")
	t.Setenv("REPOLENS_REALBENCH_RESPONSE_FORMAT", "none")
	experimentOptions := configuredRealBenchGenerationOptions()
	if experimentOptions.ReasoningEffort != "low" || experimentOptions.ResponseFormat != "none" || experimentOptions.MetadataReasoningEffort() != "low" {
		t.Fatalf("experiment options were not loaded: %+v", experimentOptions)
	}
	if got := experimentOptions.AgentOptions(); got.ReasoningEffort != "low" || got.ResponseFormat != nil {
		t.Fatalf("response_format=none was not removed from the request: %+v", got)
	}

	t.Setenv("REPOLENS_REALBENCH_RESPONSE_FORMAT", "unsupported")
	if got := configuredRealBenchGenerationOptions().ResponseFormat; got != "json_object" {
		t.Fatalf("unsupported response format = %q, want json_object", got)
	}
}

func TestRealBenchGenerationOptionsReachProviderRequest(t *testing.T) {
	provider := &providerSpy{}
	loop := agent.NewAgentLoop(provider, agent.NewToolRegistry(), nil, agent.DefaultGuardConfig()).WithGenerationOptions((realBenchGenerationOptions{
		ReasoningEffort: "low",
		ResponseFormat:  "none",
	}).AgentOptions())
	if _, err := loop.Run(context.Background(), &diagnosis.DiagnosisRun{IssueTitle: "issue"}, &diagnosis.DiagnosisAttempt{ID: "attempt-realbench-options"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || provider.requests[0].ReasoningEffort != "low" || provider.requests[0].ResponseFormat != nil {
		t.Fatalf("RealBench generation options did not reach provider: %+v", provider.requests)
	}
}

func TestRootCauseRubricUsesGroundTruthOnlyAfterExecution(t *testing.T) {
	truth := GroundTruth{
		ExpectedRootCause: "handler returns stale cache value after refresh",
		PrimaryFiles:      []string{"handler.go"},
	}
	report := &evidence.DiagnosisReportData{
		Summary:   "The handler returns a stale cache value after refresh.",
		RootCause: "The refresh path leaves the handler cache stale.",
		Findings:  []evidence.Finding{{Citations: []evidence.Citation{{FilePath: "handler.go"}}}},
	}
	if got := gradeRootCause(report, truth); got != "Correct" {
		t.Fatalf("root cause grade = %s, want Correct", got)
	}
}

func TestCitationFailureDoesNotOverrideRootCauseGrade(t *testing.T) {
	truth := GroundTruth{ExpectedRootCause: "handler returns stale cache value after refresh", PrimaryFiles: []string{"handler.go"}}
	report := &evidence.DiagnosisReportData{
		ConclusionKind: evidence.ConclusionRootCause,
		Summary:        "The handler returns a stale cache value after refresh.",
		RootCause:      "The refresh path leaves the handler cache stale.",
		Findings:       []evidence.Finding{{Title: "stale cache", Reasoning: "The refresh path leaves the cache stale.", Citations: []evidence.Citation{{EvidenceID: "ev_unknown", ValidationStatus: evidence.CitationInvalid, ValidationError: "EVIDENCE_NOT_FOUND"}}}},
	}
	if got := gradeRootCause(report, truth); got != "Correct" {
		t.Fatalf("root cause grade = %s, want Correct despite invalid citation", got)
	}
	if unsupported := countUnsupportedClaims(report); unsupported != 1 {
		t.Fatalf("unsupported claims = %d, want 1 for invalid-only finding", unsupported)
	}
}

func TestRealBenchSeparatesCompletedE2EFromCitationGateFailure(t *testing.T) {
	metrics := aggregateMetrics([]CaseStatus{{
		CaseID: "REAL-001", Status: "RETRIEVAL_AND_E2E_COMPLETED", E2EStatus: e2eCompleted,
		ExecutionStatus: e2eCompleted, ReportStatus: string(evidence.ReportDegraded), CitationIntegrityGate: "FAILED",
		HitAt5: true, HitAt10: true, ReciprocalRank: 1,
	}})
	if metrics.CompletedCases != 1 || metrics.InfraErrors != 0 || metrics.ProductFailures != 0 || metrics.CitationGateFailures != 1 {
		t.Fatalf("unexpected separated metrics: %+v", metrics)
	}
}

func TestFailedE2EMetricsPreserveExternalClassification(t *testing.T) {
	metrics := failedE2EMetrics(externalFailure("provider preflight", errors.New("429")))
	if metrics.Status != e2eFailure || metrics.FailureClassification != string(failureExternalInfra) || metrics.ReportStatus != "NOT_AVAILABLE" || metrics.CitationIntegrityGate != "NOT_RUN" {
		t.Fatalf("unexpected preflight failure metrics: %+v", metrics)
	}
}

func TestFailedE2EMetricsPreserveTruncationClassification(t *testing.T) {
	metrics := failedE2EMetrics(productFailure("Agent runtime", agent.ErrModelOutputTruncated))
	if metrics.Status != e2eFailure || metrics.FailureClassification != string(failureRepolensProduct) || metrics.ErrorCode != agent.ErrCodeModelOutputTruncated || metrics.RootCauseGrade != "Not Scorable" || metrics.ReportStatus != "NOT_AVAILABLE" || metrics.CitationIntegrityGate != "NOT_RUN" {
		t.Fatalf("unexpected truncation metrics: %+v", metrics)
	}
}

func TestRunnerRecordsProviderPreflightFailureAsExternalE2EFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"quota exhausted","code":1310}}`))
	}))
	defer server.Close()
	t.Setenv("REPOLENS_REALBENCH_PROVIDER", "test-provider")
	t.Setenv("REPOLENS_REALBENCH_BASE_URL", server.URL)
	t.Setenv("REPOLENS_REALBENCH_MODEL", "test-model")
	t.Setenv("REPOLENS_REALBENCH_API_KEY", "test-key")

	datasetRoot := writeSyntheticDataset(t)
	dataset, err := LoadInputs(datasetRoot)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(dataset)
	runner.Fetcher = syntheticFetcher{}
	result, err := runner.Run(context.Background(), RunOptions{
		CaseIDs:      []string{"REAL-999"},
		CacheDir:     filepath.Join(t.TempDir(), "cache"),
		ArtifactRoot: filepath.Join(t.TempDir(), "artifacts"),
		RunE2E:       true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Metadata.E2EStatus != e2eFailure || result.Metrics.InfraErrors != 1 || result.Metrics.ProductFailures != 0 {
		t.Fatalf("unexpected preflight failure result: metadata=%+v metrics=%+v", result.Metadata, result.Metrics)
	}
	if len(result.Cases) != 1 || result.Cases[0].ExecutionStatus != e2eFailure || result.Cases[0].ErrorClass != string(failureExternalInfra) || result.Cases[0].ReportStatus != "NOT_AVAILABLE" {
		t.Fatalf("unexpected case status: %+v", result.Cases)
	}
	metricsData, err := os.ReadFile(filepath.Join(result.RunDir, "cases", "REAL-999", "e2e_metrics.json"))
	if err != nil || !strings.Contains(string(metricsData), `"failure_classification": "EXTERNAL_INFRA"`) {
		t.Fatalf("missing external E2E metrics: err=%v data=%s", err, metricsData)
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
		Message: llm.Message{Role: llm.RoleAssistant, Content: `{"conclusion_kind":"ROOT_CAUSE","summary":"ok","root_cause":"input-only","findings":[{"title":"input","reasoning":"input-only"}],"recommended_checks":[],"confidence":0.1}`},
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
