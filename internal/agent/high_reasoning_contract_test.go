package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/analysispipeline"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	platformconfig "repolens/internal/platform/config"
	"repolens/internal/platform/mysql"
	"repolens/internal/provider"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

func TestRuntimeGenerationHTTPContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		effort    string
		tokens    int
		timeout   int
		status    int
		response  string
		truncated bool
	}{
		{"current default", "low", 4096, 60, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"{\"conclusion_kind\":\"ROOT_CAUSE\",\"summary\":\"summary\",\"root_cause\":\"root cause\",\"findings\":[{\"title\":\"finding\",\"reasoning\":\"reasoning\"}]}"},"finish_reason":"stop"}]}`, false},
		{"high experiment", "high", 8192, 180, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"{\"conclusion_kind\":\"ROOT_CAUSE\",\"summary\":\"summary\",\"root_cause\":\"root cause\",\"findings\":[{\"title\":\"finding\",\"reasoning\":\"reasoning\"}]}"},"finish_reason":"stop"}]}`, false},
		{"high reasoning exhausts completion", "high", 8192, 180, http.StatusOK, `{"choices":[{"message":{"role":"assistant","content":"{}"},"finish_reason":"length"}],"usage":{"completion_tokens":8192,"completion_tokens_details":{"reasoning_tokens":8100}}}`, true},
		{"high rejected", "high", 8192, 180, http.StatusBadRequest, `{"error":{"message":"unsupported reasoning_effort"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests []llm.GenerateRequest
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request llm.GenerateRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests = append(requests, request)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.response))
			}))
			defer server.Close()

			run := compatibleRuntimeDiagnosisRun("generation-http")
			run.ReasoningEffort, run.MaxOutputTokens, run.ProviderTimeoutSeconds = tc.effort, tc.tokens, tc.timeout
			run.NormalizedBaseURL, run.ModelName = server.URL, "contract-model"
			spec := testExecutionSpec(run)
			// The manager's new defaults must not replace the run's frozen timeout.
			manager := provider.NewManagerWithAuthModeAndTimeoutAndRetries(
				t.TempDir()+"/provider.json", server.URL, run.ModelName, "mock-key", "openai", "bearer", time.Second, 0)
			executor := NewAgentRuntimeExecutorWithFactory(manager, nil, nil, nil, DefaultGuardConfig())
			result, err := executor.Execute(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "generation-http-attempt"})
			if tc.truncated {
				if !errors.Is(err, ErrModelOutputTruncated) || result == nil || result.ParseError != ErrCodeModelOutputTruncated || result.ReasoningTokens != 8100 || result.StructuredReport {
					t.Fatalf("reasoning truncation result=%+v error=%v", result, err)
				}
			} else if tc.status != http.StatusOK {
				class, _ := jobs.ClassifyError(err)
				if class != jobs.ErrorClassPermanent {
					t.Fatalf("provider rejection = %v, class=%s", err, class)
				}
			} else if err != nil || result == nil || !result.StructuredReport {
				t.Fatalf("generation result=%+v error=%v", result, err)
			}
			if len(requests) != 1 {
				t.Fatalf("provider calls=%d, want one without downgrade/replay", len(requests))
			}
			request := requests[0]
			if request.Model != run.ModelName || request.ReasoningEffort != tc.effort || request.MaxTokens != tc.tokens || request.Temperature == nil || *request.Temperature != 0.1 || request.ResponseFormat == nil || request.ResponseFormat.Type != "json_object" || len(request.Tools) != 5 {
				t.Fatalf("generation request=%+v", request)
			}
		})
	}
}

func TestNewHighRunAndExistingLowRunKeepPersistedGeneration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "generation.sqlite")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatal(err)
	}
	for _, value := range []interface{}{
		&repo.Repository{ID: "generation-repo", UserID: "user", Name: "generation", GitURL: "https://example.test/repo"},
		&snapshot.RepositorySnapshot{ID: "generation-snapshot", RepositoryID: "generation-repo", CommitSHA: "commit", Status: snapshot.StatusReady},
		&codeintelmodel.CodeIndexBuild{ID: 1, SnapshotID: "generation-snapshot", Status: codeintelmodel.BuildStatusReady},
		&codeintelmodel.RetrievalBuild{ID: 2, CodeIndexBuildID: 1, Strategy: codeintelmodel.StrategyBM25, Status: codeintelmodel.BuildStatusReady},
	} {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	store := diagnosis.NewStore(db)
	newService := func() *diagnosis.Service {
		cfg := platformconfig.Load()
		return diagnosis.NewService(diagnosis.ServiceDependencies{
			Store: store, RepoStore: repo.NewStore(db),
			Lineage: analysispipeline.NewResolver(revision.NewStore(db), snapshot.NewStore(db), codeintelstore.NewStore(db)),
			ProviderSource: func() diagnosis.ProviderMetadata {
				return diagnosis.ProviderMetadata{
					IsConfigured: true, NormalizedBaseURL: "https://provider.example/v1", ModelName: "frozen-model",
					EndpointFingerprint: "endpoint", ConfigFingerprint: "config",
					PromptVersion: diagnosis.CurrentPromptVersion, AgentVersion: diagnosis.CurrentAgentVersion, Temperature: 0.1,
					ReasoningEffort: cfg.ReasoningEffort, MaxOutputTokens: cfg.MaxOutputTokens,
					ProviderTimeoutSeconds: cfg.ProviderTimeoutSeconds, ProviderRetryAttempts: 0,
				}
			},
		})
	}
	ctx := context.Background()
	input := diagnosis.CreateDiagnosisInput{
		UserID: "user", RepositoryID: "generation-repo", SnapshotID: "generation-snapshot",
		CodeIndexBuildID: 1, RetrievalBuildID: 2, IssueTitle: "issue", IdempotencyKey: "low-run",
	}
	t.Setenv("REPOLENS_REASONING_EFFORT", "low")
	t.Setenv("REPOLENS_MAX_OUTPUT_TOKENS", "4096")
	t.Setenv("REPOLENS_PROVIDER_TIMEOUT_SECONDS", "60")
	lowRun, created, err := newService().Create(ctx, input)
	if err != nil || !created {
		t.Fatalf("create low run: created=%t error=%v", created, err)
	}
	t.Setenv("REPOLENS_REASONING_EFFORT", "high")
	t.Setenv("REPOLENS_MAX_OUTPUT_TOKENS", "8192")
	t.Setenv("REPOLENS_PROVIDER_TIMEOUT_SECONDS", "180")
	restartedService := newService()
	duplicate, created, err := restartedService.Create(ctx, input)
	if err != nil || created || duplicate.ID != lowRun.ID {
		t.Fatalf("reuse old run after config change: created=%t error=%v", created, err)
	}
	input.IdempotencyKey = "high-run"
	highRun, created, err := restartedService.Create(ctx, input)
	if err != nil || !created {
		t.Fatalf("create high run: created=%t error=%v", created, err)
	}
	if lowRun.AgentConfigHash == highRun.AgentConfigHash {
		t.Fatal("different frozen generation settings share a hash")
	}
	for _, tc := range []struct {
		run     *diagnosis.DiagnosisRun
		effort  string
		tokens  int
		timeout int
	}{{lowRun, "low", 4096, 60}, {highRun, "high", 8192, 180}} {
		persisted, err := diagnosis.NewStore(db).GetByID(ctx, tc.run.ID)
		if err != nil {
			t.Fatal(err)
		}
		apiSpec, err := diagnosis.BuildExecutionSpec(tc.run)
		if err != nil {
			t.Fatal(err)
		}
		workerSpec, err := diagnosis.BuildExecutionSpec(persisted)
		if err != nil || !reflect.DeepEqual(apiSpec, workerSpec) {
			t.Fatalf("persisted execution spec changed: error=%v", err)
		}
		if workerSpec.Generation.ReasoningEffort != tc.effort || workerSpec.Generation.MaxOutputTokens != tc.tokens || workerSpec.Provider.TimeoutSeconds != tc.timeout || workerSpec.Provider.NormalizedBaseURL != "https://provider.example/v1" || workerSpec.Provider.ModelName != "frozen-model" {
			t.Fatalf("incorrect frozen spec: %+v", workerSpec)
		}
		mock := &runtimeGenerationProvider{}
		if _, err := NewAgentRuntimeExecutor(mock, nil, nil, nil, DefaultGuardConfig()).Execute(ctx, workerSpec, &diagnosis.DiagnosisAttempt{ID: "attempt-" + tc.effort}); err != nil {
			t.Fatal(err)
		}
		if len(mock.requests) != 1 || mock.requests[0].ReasoningEffort != tc.effort || mock.requests[0].MaxTokens != tc.tokens || mock.requests[0].ResponseFormat == nil || mock.requests[0].ResponseFormat.Type != "json_object" {
			t.Fatalf("worker did not use frozen generation: %+v", mock.requests)
		}
	}
}
