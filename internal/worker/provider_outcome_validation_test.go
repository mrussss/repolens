package worker_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
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

func TestVC001CancelPollFailureDoesNotReplayProvider(t *testing.T) {
	db, jobStore := setupTestEnvironment(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	baseDiagnosisStore := diagnosis.NewStore(db)
	diagnosisStore := &failFirstDiagnosisFinalizer{
		Store: baseDiagnosisStore, firstFailureObserved: make(chan struct{}), allowFirstFailure: make(chan struct{}),
	}
	provider := &gatedOutcomeUnknownProvider{entered: make(chan struct{}), release: make(chan struct{})}
	executor := agent.NewAgentRuntimeExecutor(provider, nil, nil, nil, agent.DefaultGuardConfig())
	run := compatibleFC07Run("vc001-cancel-poll-outcome-unknown", "http://provider.invalid", 0)
	run.UserID = "vc001-user"
	run.RepositoryID = "vc001-repo"
	run.SnapshotID = "vc001-snapshot"
	run.IssueTitle = "post-dispatch provider outcome is unknown"
	run.IdempotencyKey = "vc001-provider-outcome-key"
	run.IdempotencyRequestHash = "vc001-provider-outcome-hash"
	if err := baseDiagnosisStore.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}

	store := &vc001CancelPollFailureStore{
		Store: jobStore, pollObserved: make(chan struct{}), failureFinalized: make(chan struct{}),
	}
	cfg := jobs.DefaultWorkerConfig()
	cfg.WorkerID = "vc001-worker"
	cfg.Concurrency = 1
	cfg.BatchSize = 1
	cfg.PollInterval = 5 * time.Millisecond
	cfg.LeaseDuration = 5 * time.Second
	cfg.ReapInterval = time.Hour
	workerRuntime := jobs.NewWorker(store, cfg)
	workerRuntime.RegisterHandler(jobs.JobTypeRunDiagnosis, agentDiagnosisHandler(diagnosisStore, db, executor))
	workerRuntime.Start(context.Background())
	t.Cleanup(func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = workerRuntime.StopGracefully(stopCtx)
	})

	select {
	case <-provider.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fake provider was not called")
	}
	claimed, err := jobStore.GetJobByResource(context.Background(), jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Status != jobs.StatusRunning || claimed.ClaimToken == nil || claimed.WorkerID == nil || claimed.MaxAttempts <= 1 || !claimed.ExecutionStarted {
		t.Fatalf("provider started without a valid RUNNING claim and retry budget: %+v", claimed)
	}
	select {
	case <-store.pollObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("cancel poll fault was not injected")
	}
	close(provider.release)
	select {
	case <-diagnosisStore.firstFailureObserved:
	case <-time.After(5 * time.Second):
		t.Fatal("FinalizeDiagnosisFailure fault was not injected")
	}
	beforeRecovery, err := jobStore.GetJobByID(context.Background(), claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeRecovery.Status != jobs.StatusRunning || beforeRecovery.ClaimToken == nil || *beforeRecovery.ClaimToken != *claimed.ClaimToken ||
		beforeRecovery.WorkerID == nil || *beforeRecovery.WorkerID != *claimed.WorkerID {
		t.Fatalf("pre-commit failure changed the active claim: %+v", beforeRecovery)
	}
	close(diagnosisStore.allowFirstFailure)
	select {
	case <-store.failureFinalized:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not finalize the reconciled provider failure")
	}

	savedJob, err := jobStore.GetJobByResource(context.Background(), jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := provider.requests.Load(); got != 1 {
		t.Fatalf("provider request count=%d; want exactly one after cancel poll and finalization faults", got)
	}
	if diagnosisStore.calls.Load() != 1 {
		t.Fatalf("FinalizeDiagnosisFailure calls=%d; want one injected pre-commit failure", diagnosisStore.calls.Load())
	}
	if savedJob.Status != jobs.StatusFailed || savedJob.LastErrorClass == nil || *savedJob.LastErrorClass != string(jobs.ErrorClassPermanent) ||
		savedJob.LastErrorCode == nil || *savedJob.LastErrorCode != llm.OutcomeUnknownErrorCode ||
		savedJob.TerminalReason == nil || *savedJob.TerminalReason != jobs.TerminalReasonPermanent {
		t.Fatalf("job after reconciliation=%+v; want terminal PERMANENT/PROVIDER_OUTCOME_UNKNOWN", savedJob)
	}
}

type gatedOutcomeUnknownProvider struct {
	requests atomic.Int32
	entered  chan struct{}
	release  chan struct{}
}

func (p *gatedOutcomeUnknownProvider) Generate(context.Context, llm.GenerateRequest) (llm.GenerateResponse, error) {
	p.requests.Add(1)
	close(p.entered)
	<-p.release
	return llm.GenerateResponse{}, &llm.OutcomeUnknownError{Cause: errors.New("provider accepted request but response was lost")}
}

type vc001CancelPollFailureStore struct {
	*jobs.Store
	pollObserved     chan struct{}
	failureFinalized chan struct{}
	pollCalls        atomic.Int32
}

func (s *vc001CancelPollFailureStore) IsCancelRequested(context.Context, int64, string, string) (bool, error) {
	if s.pollCalls.Add(1) == 1 {
		close(s.pollObserved)
		return false, errors.New("injected transient cancel-poll database error")
	}
	return false, errors.New("unexpected additional cancel poll")
}

func (s *vc001CancelPollFailureStore) ConditionalFinalizeFailure(ctx context.Context, id int64, workerID, claimToken string, class jobs.ErrorClass, code, message string, reason *jobs.TerminalReason, terminal bool, nextRun time.Time) error {
	err := s.Store.ConditionalFinalizeFailure(ctx, id, workerID, claimToken, class, code, message, reason, terminal, nextRun)
	if err == nil {
		close(s.failureFinalized)
	}
	return err
}

func TestFC07OutcomeUnknownStopsAutomaticReplayAndAllowsManualRetry(t *testing.T) {
	db, jobStore := setupTestEnvironment(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	var requests atomic.Int32
	firstRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch n := requests.Add(1); n {
		case 1:
			close(firstRequest)
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
		default:
			writeValidationFC07Success(w)
		}
	}))
	defer server.Close()

	provider := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout("", server.URL, "validation-model", "none", 3*time.Second)
	executor := agent.NewAgentRuntimeExecutor(provider, nil, nil, nil, agent.DefaultGuardConfig())
	diagnosisStore := diagnosis.NewStore(db)
	run := compatibleFC07Run("validation-fc07-outcome-unknown", server.URL, 0)
	run.UserID = "validation-fc07-user"
	run.RepositoryID = "validation-fc07-repo"
	run.SnapshotID = "validation-fc07-snapshot"
	run.IssueTitle = "FC-07 post-dispatch response loss"
	run.IdempotencyKey = "validation-fc07-outcome-unknown-key"
	run.IdempotencyRequestHash = "validation-fc07-outcome-unknown-hash"
	run.NormalizedBaseURL = server.URL
	run.ModelName = "validation-model"
	if err := diagnosisStore.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	handler := agentDiagnosisHandler(diagnosisStore, db, executor)
	_, stopWorker := startValidationFC07Worker(jobStore, handler)
	t.Cleanup(func() { _ = stopWorker() })

	select {
	case <-firstRequest:
	case <-time.After(5 * time.Second):
		t.Fatalf("first provider request was not observed; request count=%d", requests.Load())
	}
	if err := awaitValidationFC07RunFailed(diagnosisStore, run.ID); err != nil {
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
	if requests.Load() != 1 || savedJob.Status != jobs.StatusFailed || savedJob.LastErrorClass == nil || *savedJob.LastErrorClass != string(jobs.ErrorClassPermanent) ||
		savedJob.LastErrorCode == nil || *savedJob.LastErrorCode != llm.OutcomeUnknownErrorCode || len(attempts) != 1 {
		t.Fatalf("outcome-unknown evidence: provider requests=%d job=%+v attempts=%d", requests.Load(), savedJob, len(attempts))
	}
	first := attempts[0]
	if first.Status != diagnosis.AttemptStatusFailedTerminal || first.ErrorCode != llm.OutcomeUnknownErrorCode || first.ProviderCalls != 1 || first.PromptTokens != 0 || first.CompletionTokens != 0 || first.ToolCalls != 0 {
		t.Fatalf("post-dispatch attempt=%+v; want terminal PROVIDER_OUTCOME_UNKNOWN, one provider call, zero usage and tools", first)
	}
	if !jobs.IsRetryableDiagnosisProviderFailure(jobs.ErrorClassPermanent, llm.OutcomeUnknownErrorCode) {
		t.Fatal("PROVIDER_OUTCOME_UNKNOWN must allow an explicit diagnosis retry")
	}
	if err := jobStore.RetryDiagnosis(context.Background(), run.ID); err != nil {
		t.Fatalf("explicit diagnosis retry after outcome unknown: %v", err)
	}
	retriedJob, err := jobStore.GetJobByResource(context.Background(), jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil || retriedJob.Status != jobs.StatusPending || retriedJob.ExecutionGeneration != 2 {
		t.Fatalf("manual retry job=%+v err=%v; want PENDING generation 2", retriedJob, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("manual requeue unexpectedly made a provider request: requests=%d", requests.Load())
	}
}

func TestFC07OutcomeUnknownFinalizationFailureDoesNotReplayProvider(t *testing.T) {
	db, jobStore := setupTestEnvironment(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	var requests atomic.Int32
	firstRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			close(firstRequest)
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
			_ = conn.Close()
			return
		}
		writeValidationFC07Success(w)
	}))
	defer server.Close()

	provider := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout("", server.URL, "validation-model", "none", 3*time.Second)
	executor := agent.NewAgentRuntimeExecutor(provider, nil, nil, nil, agent.DefaultGuardConfig())
	baseStore := diagnosis.NewStore(db)
	faultStore := &failFirstDiagnosisFinalizer{Store: baseStore}
	run := compatibleFC07Run("validation-fc07-finalize-outcome-unknown", server.URL, 0)
	run.UserID = "validation-fc07-user"
	run.RepositoryID = "validation-fc07-repo"
	run.SnapshotID = "validation-fc07-snapshot"
	run.IssueTitle = "FC-07 outcome unknown plus atomic finalization failure"
	run.IdempotencyKey = "validation-fc07-finalize-outcome-unknown-key"
	run.IdempotencyRequestHash = "validation-fc07-finalize-outcome-unknown-hash"
	run.NormalizedBaseURL = server.URL
	run.ModelName = "validation-model"
	if err := baseStore.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	handler := agentDiagnosisHandler(faultStore, db, executor)
	_, stopWorker := startValidationFC07Worker(jobStore, handler)
	t.Cleanup(func() { _ = stopWorker() })

	select {
	case <-firstRequest:
	case <-time.After(5 * time.Second):
		t.Fatalf("first provider request was not observed; request count=%d", requests.Load())
	}
	if err := awaitValidationFC07RunFailed(baseStore, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := stopWorker(); err != nil {
		t.Fatalf("stop worker: %v", err)
	}

	savedJob, err := jobStore.GetJobByResource(context.Background(), jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	savedRun, err := baseStore.GetByID(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := baseStore.ListAttemptsByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if faultStore.calls.Load() != 1 || requests.Load() != 1 || savedJob.Status != jobs.StatusFailed ||
		savedJob.LastErrorClass == nil || *savedJob.LastErrorClass != string(jobs.ErrorClassPermanent) ||
		savedJob.LastErrorCode == nil || *savedJob.LastErrorCode != llm.OutcomeUnknownErrorCode ||
		savedJob.TerminalReason == nil || *savedJob.TerminalReason != jobs.TerminalReasonPermanent ||
		savedJob.LastErrorMessage == nil || !strings.Contains(*savedJob.LastErrorMessage, "automatic replay is disabled") ||
		savedRun.Status != diagnosis.StatusFailed || len(attempts) != 1 || attempts[0].Status != diagnosis.AttemptStatusFailedTerminal {
		t.Fatalf("combined outcome-unknown finalization evidence: finalize calls=%d provider requests=%d job=%+v run=%s attempts=%+v",
			faultStore.calls.Load(), requests.Load(), savedJob, savedRun.Status, attempts)
	}
	if !jobs.IsRetryableDiagnosisProviderFailure(jobs.ErrorClassPermanent, llm.OutcomeUnknownErrorCode) {
		t.Fatal("PROVIDER_OUTCOME_UNKNOWN must still allow an explicit diagnosis retry")
	}
}

type failFirstDiagnosisFinalizer struct {
	diagnosis.Store
	calls                atomic.Int32
	firstFailureObserved chan struct{}
	allowFirstFailure    chan struct{}
}

func (s *failFirstDiagnosisFinalizer) FinalizeDiagnosisFailure(ctx context.Context, jobID int64, workerID, claimToken string, generation int, runID, attemptID string, class jobs.ErrorClass, code, message string, promptTokens, completionTokens, toolCalls int) error {
	if s.calls.Add(1) == 1 {
		if s.firstFailureObserved != nil {
			close(s.firstFailureObserved)
			<-s.allowFirstFailure
		}
		return fmt.Errorf("injected transient failure before atomic commit")
	}
	return s.Store.FinalizeDiagnosisFailure(ctx, jobID, workerID, claimToken, generation, runID, attemptID, class, code, message, promptTokens, completionTokens, toolCalls)
}

func TestFC07Explicit429RemainsProviderRetryable(t *testing.T) {
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
	run := compatibleFC07Run("validation-fc07-429", server.URL, 1)
	run.IssueTitle = "FC-07 explicit 429"
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

func TestMalformed2xxProviderResponseDoesNotReplayJob(t *testing.T) {
	runProviderHTTPFailureJobTest(t, http.StatusOK, `{"choices":[`, 2, llm.OutcomeUnknownErrorCode, jobs.ErrorClassPermanent)
}

func TestHTTP400ProviderResponseDoesNotReplayJob(t *testing.T) {
	runProviderHTTPFailureJobTest(t, http.StatusBadRequest, `{}`, 0, "HTTP_400_BAD_REQUEST", jobs.ErrorClassPermanent)
}

func runProviderHTTPFailureJobTest(t *testing.T, status int, body string, providerRetries int, wantCode string, wantClass jobs.ErrorClass) {
	t.Helper()
	db, jobStore := setupTestEnvironment(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	var requests atomic.Int32
	firstRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Add(1) == 1 {
			close(firstRequest)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write HTTP %d provider response: %v", status, err)
		}
	}))
	defer server.Close()

	provider := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout("", server.URL, "validation-model", "none", 3*time.Second)
	executor := agent.NewAgentRuntimeExecutor(llm.NewRetryingProvider(provider, providerRetries), nil, nil, nil, agent.DefaultGuardConfig())
	diagnosisStore := diagnosis.NewStore(db)
	run := compatibleFC07Run(fmt.Sprintf("provider-http-%d-%d", status, time.Now().UnixNano()), server.URL, providerRetries)
	run.UserID = "provider-http-user"
	run.RepositoryID = "provider-http-repo"
	run.SnapshotID = "provider-http-snapshot"
	run.IssueTitle = fmt.Sprintf("provider HTTP %d classification", status)
	run.IdempotencyKey = fmt.Sprintf("provider-http-key-%d-%d", status, time.Now().UnixNano())
	run.IdempotencyRequestHash = "provider-http-request-hash"
	run.NormalizedBaseURL = server.URL
	run.ModelName = "validation-model"
	if err := diagnosisStore.Create(context.Background(), run); err != nil {
		t.Fatal(err)
	}
	createdJob, err := jobStore.GetJobByResource(context.Background(), jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if createdJob.MaxAttempts <= 1 {
		t.Fatalf("diagnosis job retry budget=%d, want greater than one", createdJob.MaxAttempts)
	}

	handler := agentDiagnosisHandler(diagnosisStore, db, executor)
	_, stopWorker := startValidationFC07Worker(jobStore, handler)
	t.Cleanup(func() { _ = stopWorker() })
	select {
	case <-firstRequest:
	case <-time.After(5 * time.Second):
		t.Fatalf("HTTP %d provider request was not observed", status)
	}
	if err := awaitValidationFC07RunFailed(diagnosisStore, run.ID); err != nil {
		t.Fatalf("HTTP %d diagnosis outcome: %v", status, err)
	}
	if err := stopWorker(); err != nil {
		t.Fatalf("stop worker after HTTP %d failure: %v", status, err)
	}

	savedJob, err := jobStore.GetJobByResource(context.Background(), jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := diagnosisStore.ListAttemptsByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("HTTP %d provider requests=%d; want exactly one", status, got)
	}
	if savedJob.Status != jobs.StatusFailed || savedJob.LastErrorClass == nil || *savedJob.LastErrorClass != string(wantClass) ||
		savedJob.LastErrorCode == nil || *savedJob.LastErrorCode != wantCode || savedJob.Status == jobs.StatusRetryWait {
		t.Fatalf("HTTP %d final job=%+v; want FAILED/%s/%s and no RETRY_WAIT", status, savedJob, wantClass, wantCode)
	}
	if len(attempts) != 1 || attempts[0].Status != diagnosis.AttemptStatusFailedTerminal || attempts[0].ErrorCode != wantCode {
		t.Fatalf("HTTP %d diagnosis attempts=%+v; want one terminal attempt with code %s", status, attempts, wantCode)
	}
}

func agentDiagnosisHandler(store diagnosis.Store, db *gorm.DB, executor agent.Executor) jobs.Handler {
	// report and citation stores are only used on successful terminal finalization.
	// The handler needs the concrete Gorm database for their constructors.
	return worker.NewDiagnosisJobHandler(store, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil, executor)
}

func compatibleFC07Run(id, baseURL string, providerRetries int) *diagnosis.DiagnosisRun {
	guard := agent.DefaultGuardConfig()
	run := &diagnosis.DiagnosisRun{
		ID: id, PromptVersion: diagnosis.CurrentPromptVersion, AgentVersion: diagnosis.CurrentAgentVersion,
		NormalizedBaseURL: baseURL, ModelName: "validation-model", ProviderTimeoutSeconds: 60,
		ProviderRetryAttempts: providerRetries, Temperature: 0.1,
		MaxAgentRounds: guard.MaxSteps, MaxToolCalls: guard.MaxToolCalls,
		MaxSearchCalls: guard.MaxSearchCalls, MaxRepeatCalls: guard.MaxRepeatCalls,
		MaxEvidencePacketBytes: 32 * 1024, MaxToolResultBytes: guard.MaxToolResultBytes,
		FinalizationTurns: 1, MaxOutputTokens: guard.MaxOutputTokens,
	}
	run.AgentConfigHash = diagnosis.ComputeAgentConfigHashWithGenerationOptions(
		run.MaxAgentRounds, run.MaxToolCalls, run.MaxSearchCalls, run.MaxRepeatCalls,
		run.MaxEvidencePacketBytes, run.MaxToolResultBytes, run.FinalizationTurns,
		run.MaxOutputTokens, run.ProviderTimeoutSeconds, run.ProviderRetryAttempts,
		run.Temperature, run.ReasoningEffort, "json_object",
	)
	return run
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

func awaitValidationFC07RunFailed(store diagnosis.Store, runID string) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := store.GetByID(context.Background(), runID)
		if err == nil && run.Status == diagnosis.StatusFailed {
			return nil
		}
		select {
		case <-deadline.C:
			return fmt.Errorf("diagnosis did not fail before timeout; last status=%v err=%v", run, err)
		case <-ticker.C:
		}
	}
}
