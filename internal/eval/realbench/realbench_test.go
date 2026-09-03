package realbench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
