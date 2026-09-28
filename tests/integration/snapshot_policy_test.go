package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/agent"
	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/indexing"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/snapshot"
	"repolens/internal/snapshotpolicy"
	"repolens/internal/tools"
)

type snapshotPolicyRepoStore struct{ repo.Store }

func (snapshotPolicyRepoStore) GetByID(_ context.Context, id string) (*repo.Repository, error) {
	return &repo.Repository{ID: id, Name: "example.com/policy", GitURL: "https://github.com/example/policy"}, nil
}

func TestSnapshotVisibilityMatchesManifestCodeIndexReadFileAndEvidence(t *testing.T) {
	ctx := context.Background()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "snapshot_policy.db?_busy_timeout=5000&_journal_mode=WAL")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	const repoID, snapshotID = "repo-policy", "snapshot-policy"
	const claimToken = "policy-materialization"
	const commitSHA = "0123456789abcdef0123456789abcdef01234567"
	basePath := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(basePath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil {
				return err
			}
			if info.IsDir() {
				return os.Chmod(path, 0755)
			}
			return os.Chmod(path, 0644)
		})
	})
	snapStore := snapshot.NewStore(db)
	if err := snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapshotID, RepositoryID: repoID, CommitSHA: commitSHA, Ref: "main",
		MaterializedPath: filepath.Join(basePath, repoID, snapshotID, "source"), Status: snapshot.StatusMaterializing,
	}); err != nil {
		t.Fatal(err)
	}
	storeFS := snapshotstore.NewLocalSnapshotStore(basePath)
	sourceRoot, err := storeFS.GetExecutionSourcePath(repoID, snapshotID, 1, claimToken)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sourceRoot, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":           "module example.com/policy\n\ngo 1.22\n",
		"src/main.go":      "package src\nfunc Visible() {}\n",
		".env":             "TOKEN=secret\n",
		".env.staging":     "TOKEN=staging-secret\n",
		".ENV.PRODUCTION":  "TOKEN=production-secret\n",
		"secret.pem":       "private key\n",
		"foo.key":          "private key\n",
		"secrets.json":     `{"password":"snapshot-supersecret"}`,
		"logs/request.log": `request_body="{\"password\":\"snapshot-supersecret\"}"`,
		"dist/main.go":     "package dist\nfunc HiddenDist() {}\n",
		"build/main.go":    "package build\nfunc HiddenBuild() {}\n",
		"README.md":        "visible documentation\n",
	}
	for relativePath, content := range files {
		fullPath := filepath.Join(sourceRoot, filepath.FromSlash(relativePath))
		if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	job := &jobs.AnalysisJob{
		ID: 1, JobType: jobs.JobTypeMaterializeSnapshot, ResourceID: snapshotID,
		ExecutionGeneration: 1, ClaimToken: stringPointer(claimToken), AttemptCount: 1, MaxAttempts: 3,
	}
	handler := indexing.NewSnapshotJobHandler(
		snapshotPolicyRepoStore{}, snapStore, nil, storeFS, &policyCloner{commitSHA: commitSHA},
		indexing.NewFileFilter(512), indexing.NewCodeChunker(60, 10), nil,
	)
	if err := handler.Execute(ctx, job); err != nil {
		t.Fatalf("materialize policy fixture: %v", err)
	}
	readySnap, err := snapStore.GetByID(ctx, snapshotID)
	if err != nil || readySnap.Status != snapshot.StatusReady {
		t.Fatalf("snapshot status=%v err=%v", readySnap.Status, err)
	}
	storeFS.WithSourcePathResolver(func(foundRepoID, foundSnapshotID string) (string, bool) {
		found, err := snapStore.GetByID(ctx, foundSnapshotID)
		if err != nil || found.RepositoryID != foundRepoID || found.Status != snapshot.StatusReady {
			return "", false
		}
		return found.MaterializedPath, true
	})
	manifest, err := snapshotpolicy.LoadManifest(readySnap.MaterializedPath)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ContentHash != readySnap.ContentHash {
		t.Fatalf("manifest content hash=%s, snapshot content hash=%s", manifest.ContentHash, readySnap.ContentHash)
	}
	manifestPaths := make(map[string]bool, len(manifest.Files))
	entries := make([]string, 0, len(manifest.Files))
	for _, entry := range manifest.Files {
		manifestPaths[entry.Path] = true
		entries = append(entries, entry.Path+"\x00"+files[entry.Path])
	}
	sort.Strings(entries)
	hasher := sha256.New()
	for _, entry := range entries {
		_, _ = hasher.Write([]byte(entry))
	}
	if got := hex.EncodeToString(hasher.Sum(nil)); got != readySnap.ContentHash {
		t.Fatalf("snapshot hash=%s, want manifest-visible hash %s", readySnap.ContentHash, got)
	}

	analysis, err := codeintel.NewAnalyzer().AnalyzeWithAllowedFiles(ctx, readySnap.MaterializedPath, manifest.AllowedPaths(), codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("analyze manifest files: %v", err)
	}
	codeIndexFiles := make(map[string]bool, len(analysis.Files))
	for _, file := range analysis.Files {
		codeIndexFiles[file.Path] = true
	}
	readTool := tools.NewReadFileTool(storeFS, repoID, snapshotID)
	evidenceIssuer := evidence.NewEvidenceIssuerWithStore(storeFS, evidence.NewEvidenceStore(db))
	tests := []struct {
		path    string
		allowed bool
	}{
		{path: ".env", allowed: false},
		{path: ".env.staging", allowed: false},
		{path: ".ENV.PRODUCTION", allowed: false},
		{path: "secret.pem", allowed: false},
		{path: "foo.key", allowed: false},
		{path: "secrets.json", allowed: false},
		{path: "dist/main.go", allowed: false},
		{path: "build/main.go", allowed: false},
		{path: "logs/request.log", allowed: true},
		{path: "src/main.go", allowed: true},
	}
	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			if got := manifestPaths[test.path]; got != test.allowed {
				t.Errorf("snapshot manifest includes=%v, want %v", got, test.allowed)
			}
			if got := codeIndexFiles[test.path]; got != (test.path == "src/main.go") {
				t.Errorf("CodeIndex includes=%v, want %v", got, test.path == "src/main.go")
			}
			_, readErr := readTool.Execute(ctx, fmt.Sprintf(`{"path":%q,"start_line":1,"end_line":1}`, test.path))
			if (readErr == nil) != test.allowed {
				t.Errorf("read_file err=%v, allowed=%v", readErr, test.allowed)
			}
			_, evidenceErr := evidenceIssuer.Issue(ctx, evidence.IssueRequest{
				AttemptID: "attempt-policy", DiagnosisRunID: "run-policy", RepositoryID: repoID,
				SnapshotID: snapshotID, CodeIndexBuildID: 1, SourceKind: evidence.SourceReadFile,
				FilePath: test.path, StartLine: 1, EndLine: 1, MaxBytes: 1024,
			})
			if (evidenceErr == nil) != test.allowed {
				t.Errorf("Evidence err=%v, allowed=%v", evidenceErr, test.allowed)
			}
		})
	}
	logEvidence, err := evidenceIssuer.Issue(ctx, evidence.IssueRequest{
		AttemptID: "attempt-policy-escaped-log", DiagnosisRunID: "run-policy-escaped-log", RepositoryID: repoID,
		SnapshotID: snapshotID, CodeIndexBuildID: 1, SourceKind: evidence.SourceReadFile,
		FilePath: "logs/request.log", StartLine: 1, EndLine: 1, MaxBytes: 1024,
	})
	if err != nil {
		t.Fatalf("issue escaped-log evidence: %v", err)
	}
	if strings.Contains(logEvidence.DisplayExcerpt, "snapshot-supersecret") || !strings.Contains(logEvidence.DisplayExcerpt, "request_body") || !strings.Contains(logEvidence.DisplayExcerpt, "[REDACTED_SECRET]") {
		t.Fatalf("escaped-log Evidence content=%q; want retained context and redacted credential", logEvidence.DisplayExcerpt)
	}

	provider := &snapshotPolicyCaptureProvider{}
	registry := agent.NewToolRegistry()
	providerReadTool := tools.NewReadFileTool(storeFS, repoID, snapshotID).WithEvidenceIssuer(
		evidenceIssuer, "attempt-policy-provider", "run-policy-provider", 1, 1024,
	)
	registry.Register(providerReadTool)
	loop := agent.NewAgentLoop(provider, registry, nil, agent.DefaultGuardConfig())
	_, err = loop.Run(ctx, integrationExecutionSpec(&diagnosis.DiagnosisRun{
		ID: "run-policy-provider", RepositoryID: repoID, SnapshotID: snapshotID,
		IssueTitle: "inspect credentials file",
	}), &diagnosis.DiagnosisAttempt{ID: "attempt-policy-provider"})
	if err != nil {
		t.Fatalf("run provider boundary check: %v", err)
	}
	if len(provider.requests) != 3 {
		t.Fatalf("provider calls=%d, want initial call, two tool attempts, and final response", len(provider.requests))
	}
	redactedLogReachedProvider := false
	for callIndex, request := range provider.requests {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, "snapshot-supersecret") {
				t.Fatalf("provider call %d received excluded file content: %+v", callIndex+1, message)
			}
			if strings.Contains(message.Content, "request_body") && strings.Contains(message.Content, "[REDACTED_SECRET]") {
				redactedLogReachedProvider = true
			}
		}
	}
	if !redactedLogReachedProvider {
		t.Fatal("provider did not receive the ordinary log context with its credential redacted")
	}
}

type snapshotPolicyCaptureProvider struct {
	requests []llm.GenerateRequest
}

func (p *snapshotPolicyCaptureProvider) Generate(_ context.Context, request llm.GenerateRequest) (llm.GenerateResponse, error) {
	copyRequest := request
	copyRequest.Messages = append([]llm.Message(nil), request.Messages...)
	for i := range copyRequest.Messages {
		copyRequest.Messages[i].ToolCalls = append([]llm.ToolCall(nil), request.Messages[i].ToolCalls...)
	}
	p.requests = append(p.requests, copyRequest)
	if len(p.requests) == 1 {
		var call llm.ToolCall
		call.ID = "read-secrets-file"
		call.Type = "function"
		call.Function.Name = "read_file"
		call.Function.Arguments = `{"path":"secrets.json","start_line":1,"end_line":1}`
		return llm.GenerateResponse{
			Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}},
			FinishReason: "tool_calls",
		}, nil
	}
	if len(p.requests) == 2 {
		var call llm.ToolCall
		call.ID = "read-embedded-log"
		call.Type = "function"
		call.Function.Name = "read_file"
		call.Function.Arguments = `{"path":"logs/request.log","start_line":1,"end_line":1}`
		return llm.GenerateResponse{
			Message:      llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{call}},
			FinishReason: "tool_calls",
		}, nil
	}
	return llm.GenerateResponse{
		Message:      llm.Message{Role: llm.RoleAssistant, Content: `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}`},
		FinishReason: "stop",
	}, nil
}

type policyCloner struct{ commitSHA string }

func (c *policyCloner) ValidateGitURL(string) error { return nil }
func (c *policyCloner) CloneTo(context.Context, string, string, string) (string, error) {
	return c.commitSHA, fmt.Errorf("policy fixture should already be materialized")
}

func stringPointer(value string) *string { return &value }
