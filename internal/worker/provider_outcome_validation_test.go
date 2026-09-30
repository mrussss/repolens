package worker_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"

	"repolens/internal/agent"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	"repolens/internal/worker"
)

const validationFC07Report = `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}`

// VALIDATION-ONLY: convert this observation to a desired-invariant regression
// test during production hardening.
func TestValidationFC07OutcomeUnknownReplaysDiagnosisJob(t *testing.T) {
	db, jobStore := setupTestEnvironment(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	var requests atomic.Int32
	secondRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch n := requests.Add(1); n {
		case 1:
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("test HTTP server does not support hijacking")
				return
			}
			conn, _, hijackErr := hijacker.Hijack()
			if hijackErr != nil {
				t.Errorf("hijack first provider response: %v", hijackErr)
				return
			}
			_ = conn.Close() // The provider request arrived; its response is lost.
		case 2:
			close(secondRequest)
			writeValidationFC07Success(w)
		default:
			t.Errorf("unexpected provider request %d", n)
			writeValidationFC07Success(w)
		}
	}))
	defer server.Close()

	provider := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout("", server.URL, "validation-model", "none", 3*time.Second)
	executor := agent.NewAgentRuntimeExecutor(provider, nil, nil, nil, agent.DefaultGuardConfig())
	diagnosisStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "validation-fc07-outcome-unknown", UserID: "validation-fc07-user", RepositoryID: "validation-fc07-repo",
		SnapshotID: "validation-fc07-snapshot", IssueTitle: "FC-07 post-dispatch response loss",
		IdempotencyKey: "validation-fc07-outcome-unknown-key", IdempotencyRequestHash: "validation-fc07-outcome-unknown-hash",
		NormalizedBaseURL: server.URL, ModelName: "validation-model", ProviderRetryAttempts: 0,
	}
	if err := diagnosisStore.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	handler := agentDiagnosisHandler(diagnosisStore, db, executor)
	_, stopWorker := startValidationFC07Worker(jobStore, handler)
	t.Cleanup(func() { _ = stopWorker() })

	select {
	case <-secondRequest:
	case <-time.After(5 * time.Second):
		t.Fatalf("second provider request was not observed; request count=%d", requests.Load())
	}
	if err := awaitValidationFC07RunSucceeded(diagnosisStore, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := stopWorker(); err != nil {
		t.Fatalf("stop worker: %v", err)
	}

	savedJob, err := jobStore.GetJobByResource(context.Background(), jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := diagnosisStore.ListAttemptsByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || savedJob.Status != jobs.StatusSucceeded || len(attempts) != 2 {
		t.Fatalf("automatic retry evidence: provider requests=%d job=%s attempts=%d", requests.Load(), savedJob.Status, len(attempts))
	}
	first := attempts[0]
	if first.Status != diagnosis.AttemptStatusFailedRetryable || first.ProviderCalls != 1 || first.PromptTokens != 0 || first.CompletionTokens != 0 || first.ToolCalls != 0 {
		t.Fatalf("first post-dispatch attempt=%+v; want retryable, one provider call, zero usage and tools", first)
	}
	if attempts[1].Status != diagnosis.AttemptStatusSucceeded {
		t.Fatalf("automatic retry attempt status=%s, want SUCCEEDED", attempts[1].Status)
	}
	t.Logf("observation: first request reached fake upstream and lost its response; persisted attempt provider_calls=%d prompt_tokens=%d completion_tokens=%d tool_calls=%d; Diagnosis job automatically made request 2 and succeeded", first.ProviderCalls, first.PromptTokens, first.CompletionTokens, first.ToolCalls)
}

func TestValidationFC07Explicit429RemainsProviderRetryable(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
			return
		}
		writeValidationFC07Success(w)
	}))
	defer server.Close()

	base := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout("", server.URL, "validation-model", "none", 3*time.Second)
	provider := llm.NewRetryingProvider(base, 1)
	executor := agent.NewAgentRuntimeExecutor(provider, nil, nil, nil, agent.DefaultGuardConfig())
	run := &diagnosis.DiagnosisRun{ID: "validation-fc07-429", IssueTitle: "FC-07 explicit 429", ProviderRetryAttempts: 1}
	spec, err := diagnosis.BuildExecutionSpec(run)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.Execute(context.Background(), spec, &diagnosis.DiagnosisAttempt{ID: "validation-fc07-429-attempt"})
	if err != nil {
		t.Fatalf("runtime execution after retryable 429: %v", err)
	}
	if requests.Load() != 2 || result.ProviderCalls != 2 || result.PromptTokens != 0 || result.CompletionTokens != 0 {
		t.Fatalf("429 retry evidence: HTTP requests=%d runtime provider calls=%d usage=%d/%d", requests.Load(), result.ProviderCalls, result.PromptTokens, result.CompletionTokens)
	}
	t.Logf("observation: explicit HTTP 429 was recognized and retried; requests=%d runtime provider_calls=%d", requests.Load(), result.ProviderCalls)
}

func agentDiagnosisHandler(store diagnosis.Store, db *gorm.DB, executor agent.Executor) jobs.Handler {
	// report and citation stores are only used on successful terminal finalization.
	// The handler needs the concrete Gorm database for their constructors.
	return worker.NewDiagnosisJobHandler(store, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil, executor)
}

func writeValidationFC07Success(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, validationFC07Report)
}

func startValidationFC07Worker(store *jobs.Store, handler jobs.Handler) (*jobs.Worker, func() error) {
	cfg := jobs.DefaultWorkerConfig()
	cfg.WorkerID = "validation-fc07-worker"
	cfg.Concurrency = 1
	cfg.BatchSize = 1
	cfg.PollInterval = 5 * time.Millisecond
	cfg.LeaseDuration = 3 * time.Second
	cfg.ReapInterval = time.Hour
	cfg.BaseBackoff = time.Millisecond
	cfg.MaxBackoff = 2 * time.Millisecond
	workerRuntime := jobs.NewWorker(store, cfg)
	workerRuntime.RegisterHandler(jobs.JobTypeRunDiagnosis, handler)
	workerRuntime.Start(context.Background())

	var stopped atomic.Bool
	stop := func() error {
		if !stopped.CompareAndSwap(false, true) {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return workerRuntime.StopGracefully(ctx)
	}
	return workerRuntime, stop
}

func awaitValidationFC07RunSucceeded(store diagnosis.Store, runID string) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := store.GetByID(context.Background(), runID)
		if err == nil && run.Status == diagnosis.StatusSucceeded {
			return nil
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("diagnosis did not succeed before timeout; last status=%v err=%v", run, err)
		case <-ticker.C:
		}
	}
}
