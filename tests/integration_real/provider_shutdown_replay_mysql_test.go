package integration_real

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"repolens/internal/agent"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	"repolens/internal/worker"
)

const f01SuccessfulProviderReport = `{"conclusion_kind":"ROOT_CAUSE","summary":"summary","root_cause":"root cause","findings":[{"title":"finding","reasoning":"reasoning"}]}`

func TestRealMySQL_DiagnosisShutdownAfterProviderDispatchDoesNotAutoReplay(t *testing.T) {
	db, jobStore, cleanup := setupRealMySQL(t)
	defer cleanup()

	var providerCalls atomic.Int32
	firstRequestEntered := make(chan struct{})
	firstRequestCancelled := make(chan struct{})
	secondRequestEntered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		switch call := providerCalls.Add(1); call {
		case 1:
			close(firstRequestEntered)
			<-r.Context().Done()
			close(firstRequestCancelled)
		case 2:
			close(secondRequestEntered)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`, f01SuccessfulProviderReport)
		default:
			http.Error(w, "unexpected additional Provider call", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	provider := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout("", server.URL, "f01-model", "none", 30*time.Second)
	baseExecutor := agent.NewAgentRuntimeExecutor(provider, nil, nil, nil, agent.DefaultGuardConfig())
	observedExecutor := &f01ObservedExecutor{delegate: baseExecutor, events: make(chan f01ExecutionEvent, 3)}
	diagnosisStore := diagnosis.NewStore(db)
	run := f01DiagnosisRun(server.URL)
	if err := diagnosisStore.Create(context.Background(), run); err != nil {
		t.Fatalf("create MySQL Diagnosis and Job: %v", err)
	}

	handler := worker.NewDiagnosisJobHandler(
		diagnosisStore,
		evidence.NewReportStore(db),
		evidence.NewCitationStore(db),
		nil,
		observedExecutor,
	)
	workerOne := startF01Worker(jobStore, "f01-worker-one", handler, 800*time.Millisecond)
	workerOneStopped := false
	t.Cleanup(func() {
		if !workerOneStopped {
			stopF01Worker(t, workerOne)
		}
	})

	select {
	case <-firstRequestEntered:
		t.Log("T3: Provider HTTP handler received request #1")
	case <-time.After(10 * time.Second):
		t.Fatal("T3: Provider HTTP request did not enter")
	}

	jobBeforeShutdown, err := jobStore.GetJobByResource(context.Background(), jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatalf("read claimed MySQL Job: %v", err)
	}
	t.Logf("T1-T2: Job before shutdown: status=%s execution_started=%t attempt=%d max_attempts=%d worker=%s generation=%d",
		jobBeforeShutdown.Status, jobBeforeShutdown.ExecutionStarted, jobBeforeShutdown.AttemptCount,
		jobBeforeShutdown.MaxAttempts, f01OptionalString(jobBeforeShutdown.WorkerID), jobBeforeShutdown.ExecutionGeneration)
	if jobBeforeShutdown.Status != jobs.StatusRunning || !jobBeforeShutdown.ExecutionStarted || jobBeforeShutdown.AttemptCount != 1 || jobBeforeShutdown.MaxAttempts < 2 {
		t.Fatalf("Provider entered without a started, retryable MySQL claim: %+v", jobBeforeShutdown)
	}

	shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 100*time.Millisecond)
	shutdownErr := workerOne.StopGracefully(shutdownCtx)
	cancelShutdown()
	if !errors.Is(shutdownErr, context.DeadlineExceeded) {
		t.Fatalf("Worker #1 shutdown = %v, want grace deadline followed by active-handler cancellation", shutdownErr)
	}
	t.Logf("T4: Worker #1 graceful shutdown reached deadline: %v", shutdownErr)
	select {
	case <-firstRequestCancelled:
		t.Log("T5: Provider server observed cancellation of the already-entered HTTP request")
	case <-time.After(5 * time.Second):
		t.Fatal("T5: in-flight Provider request did not observe shutdown cancellation")
	}
	stopF01Worker(t, workerOne)
	workerOneStopped = true
	select {
	case event := <-observedExecutor.events:
		t.Logf("T5-T6: executor result: cause_is_worker_shutdown=%t outcome_unknown=%t error=%q",
			event.workerShutdown, event.outcomeUnknown, event.err)
		if !event.workerShutdown || !event.outcomeUnknown {
			t.Fatalf("shutdown did not overlap a real OutcomeUnknown result: %+v", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("T6: production Agent executor did not return after Provider cancellation")
	}

	jobAfterShutdown, err := jobStore.GetJobByID(context.Background(), jobBeforeShutdown.ID)
	if err != nil {
		t.Fatalf("read Job after Worker #1 stopped: %v", err)
	}
	runAfterShutdown, err := diagnosisStore.GetByID(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("read Diagnosis after Worker #1 stopped: %v", err)
	}
	attemptsAfterShutdown, err := diagnosisStore.ListAttemptsByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("read Attempts after Worker #1 stopped: %v", err)
	}
	t.Logf("T7: after shutdown: job={status:%s execution_started:%t attempt:%d error_class:%s error_code:%s terminal_reason:%s lease_until:%v} run=%s attempts=%+v provider_calls=%d",
		jobAfterShutdown.Status, jobAfterShutdown.ExecutionStarted, jobAfterShutdown.AttemptCount,
		f01OptionalString(jobAfterShutdown.LastErrorClass), f01OptionalString(jobAfterShutdown.LastErrorCode), f01OptionalTerminalReason(jobAfterShutdown.TerminalReason),
		jobAfterShutdown.LeaseUntil, runAfterShutdown.Status, attemptsAfterShutdown, providerCalls.Load())
	if providerCalls.Load() != 1 {
		t.Fatalf("expected exactly one Provider request before recovery, got %d", providerCalls.Load())
	}
	if jobAfterShutdown.Status == jobs.StatusRunning {
		if !jobAfterShutdown.ExecutionStarted || jobAfterShutdown.AttemptCount != 1 || len(attemptsAfterShutdown) != 1 {
			t.Fatalf("unexpected RUNNING state after started Provider request: job=%+v attempts=%+v", jobAfterShutdown, attemptsAfterShutdown)
		}
		firstAttempt := attemptsAfterShutdown[0]
		if runAfterShutdown.Status != diagnosis.StatusRunning || firstAttempt.Status != diagnosis.AttemptStatusRunning || firstAttempt.ErrorCode != "" || firstAttempt.ProviderCalls != 1 {
			t.Fatalf("RUNNING Job did not retain the expected nonterminal Diagnosis state: run=%+v attempt=%+v", runAfterShutdown, firstAttempt)
		}
		if jobAfterShutdown.LeaseUntil == nil {
			t.Fatal("T8: RUNNING Job has no lease expiry")
		}
		waitUntilF01LeaseExpires(t, jobStore, jobAfterShutdown.ID)
		reaped, reapErr := jobStore.ReapExpiredJobs(context.Background(), 10)
		if reapErr != nil {
			t.Fatalf("T9: ReapExpiredJobs on real MySQL: %v", reapErr)
		}
		afterReap, getErr := jobStore.GetJobByID(context.Background(), jobAfterShutdown.ID)
		if getErr != nil {
			t.Fatalf("read Job after ReapExpiredJobs: %v", getErr)
		}
		runAfterReap, runGetErr := diagnosisStore.GetByID(context.Background(), run.ID)
		if runGetErr != nil {
			t.Fatalf("read Diagnosis after ReapExpiredJobs: %v", runGetErr)
		}
		attemptsAfterReap, attemptsGetErr := diagnosisStore.ListAttemptsByRun(context.Background(), run.ID)
		if attemptsGetErr != nil {
			t.Fatalf("read Diagnosis Attempts after ReapExpiredJobs: %v", attemptsGetErr)
		}
		if reaped != 1 {
			t.Fatalf("T9: ReapExpiredJobs reaped %d jobs, want 1", reaped)
		}
		t.Logf("T9: real MySQL ReapExpiredJobs reaped=%d; job={status:%s started:%t attempt:%d next_run_at:%s error_class:%s error_code:%s} run=%s attempts=%+v",
			reaped, afterReap.Status, afterReap.ExecutionStarted, afterReap.AttemptCount, afterReap.NextRunAt,
			f01OptionalString(afterReap.LastErrorClass), f01OptionalString(afterReap.LastErrorCode), runAfterReap.Status, attemptsAfterReap)
		if afterReap.Status != jobs.StatusRetryWait {
			t.Fatalf("T9: expired started Job status = %s, want RETRY_WAIT", afterReap.Status)
		}
		if runAfterReap.Status != diagnosis.StatusRunning || len(attemptsAfterReap) != 1 ||
			attemptsAfterReap[0].Status != diagnosis.AttemptStatusAbandoned || attemptsAfterReap[0].ErrorCode != "LEASE_EXPIRED" ||
			attemptsAfterReap[0].ProviderCalls != 1 {
			t.Fatalf("T9: unexpected Diagnosis state after lease recovery: run=%+v attempts=%+v", runAfterReap, attemptsAfterReap)
		}
		waitUntilF01RetryDue(t, jobStore, afterReap.ID)
		workerTwo := startF01Worker(jobStore, "f01-worker-two", handler, 3*time.Second)
		secondStopped := false
		t.Cleanup(func() {
			if !secondStopped {
				stopF01Worker(t, workerTwo)
			}
		})
		select {
		case <-secondRequestEntered:
			t.Log("T10: Worker #2 entered a second Provider HTTP request after lease recovery")
		case <-time.After(10 * time.Second):
			t.Fatal("T10: Worker #2 did not claim and dispatch the recovered Job")
		}
		if err := waitForF01TerminalJob(jobStore, afterReap.ID, 10*time.Second); err != nil {
			t.Fatalf("wait for Worker #2 outcome: %v", err)
		}
		stopF01Worker(t, workerTwo)
		secondStopped = true
	} else if jobAfterShutdown.Status == jobs.StatusFailed {
		if !jobAfterShutdown.ExecutionStarted || jobAfterShutdown.LastErrorClass == nil || *jobAfterShutdown.LastErrorClass != string(jobs.ErrorClassPermanent) ||
			jobAfterShutdown.LastErrorCode == nil || *jobAfterShutdown.LastErrorCode != llm.OutcomeUnknownErrorCode ||
			jobAfterShutdown.TerminalReason == nil || *jobAfterShutdown.TerminalReason != jobs.TerminalReasonPermanent {
			t.Fatalf("terminalized ambiguous Provider outcome with unexpected Job metadata: %+v", jobAfterShutdown)
		}
		if runAfterShutdown.Status != diagnosis.StatusFailed || len(attemptsAfterShutdown) != 1 || attemptsAfterShutdown[0].Status != diagnosis.AttemptStatusFailedTerminal ||
			attemptsAfterShutdown[0].ErrorCode != llm.OutcomeUnknownErrorCode {
			t.Fatalf("terminalized ambiguous Provider outcome with unexpected Diagnosis state: run=%+v attempts=%+v", runAfterShutdown, attemptsAfterShutdown)
		}
		t.Log("T7: ambiguous Provider outcome was terminalized; no lease recovery should replay it")
	} else {
		t.Fatalf("unexpected Job state after ambiguous Provider outcome: %+v", jobAfterShutdown)
	}

	finalJob, err := jobStore.GetJobByID(context.Background(), jobBeforeShutdown.ID)
	if err != nil {
		t.Fatalf("read final Job: %v", err)
	}
	finalRun, err := diagnosisStore.GetByID(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("read final Diagnosis: %v", err)
	}
	finalAttempts, err := diagnosisStore.ListAttemptsByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("read final Attempts: %v", err)
	}
	t.Logf("T11: final state: provider_calls=%d job={status:%s generation:%d attempt:%d error_class:%s error_code:%s terminal_reason:%s} run=%s attempts=%+v",
		providerCalls.Load(), finalJob.Status, finalJob.ExecutionGeneration, finalJob.AttemptCount,
		f01OptionalString(finalJob.LastErrorClass), f01OptionalString(finalJob.LastErrorCode), f01OptionalTerminalReason(finalJob.TerminalReason), finalRun.Status, finalAttempts)

	if providerCalls.Load() != 1 {
		t.Errorf("provider call count: got %d, want 1; automatic replay occurred after an ambiguous provider outcome", providerCalls.Load())
	}
}

func TestRealMySQL_ReaperTerminalizesDurableProviderOutcomeUnknown(t *testing.T) {
	db, jobStore, cleanup := setupRealMySQL(t)
	defer cleanup()
	ctx := context.Background()
	diagnosisStore := diagnosis.NewStore(db)
	run := f01DiagnosisRun("http://provider.invalid")
	run.ID = "f01-reaper-provider-outcome-unknown"
	run.IdempotencyKey = "f01-reaper-provider-outcome-unknown-key"
	run.IdempotencyRequestHash = "f01-reaper-provider-outcome-unknown-hash"
	if err := diagnosisStore.Create(ctx, run); err != nil {
		t.Fatalf("create MySQL Diagnosis and Job: %v", err)
	}
	job, err := jobStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimJobs(ctx, "f01-crashed-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 || claimed[0].ID != job.ID {
		t.Fatalf("claim Diagnosis Job: claimed=%+v err=%v", claimed, err)
	}
	if attemptNo, err := jobStore.MarkExecutionStarted(ctx, job.ID, "f01-crashed-worker", *claimed[0].ClaimToken, claimed[0].ExecutionGeneration); err != nil || attemptNo != 1 {
		t.Fatalf("mark MySQL execution started: attempt=%d err=%v", attemptNo, err)
	}
	now := time.Now().UTC()
	attempt := &diagnosis.DiagnosisAttempt{
		ID: "f01-reaper-unknown-attempt", DiagnosisRunID: run.ID,
		ExecutionGeneration: claimed[0].ExecutionGeneration, AttemptNo: 1,
		WorkerID: "f01-crashed-worker", Status: diagnosis.AttemptStatusRunning,
		StartedAt: now, HeartbeatAt: now, DeadlineAt: now.Add(time.Minute),
	}
	if err := diagnosisStore.StartAttempt(ctx, run.ID, attempt); err != nil {
		t.Fatalf("start MySQL DiagnosisAttempt: %v", err)
	}
	if err := diagnosisStore.UpdateAttemptCheckpoint(ctx, attempt.ID, diagnosis.AttemptCheckpoint{
		ExecutionGeneration: claimed[0].ExecutionGeneration,
		Kind:                diagnosis.CheckpointKindPartialProviderFailure,
		ProviderCalls:       1,
		ErrorCode:           llm.OutcomeUnknownErrorCode,
		ErrorMessage:        "provider request outcome could not be confirmed; automatic replay is disabled",
	}); err != nil {
		t.Fatalf("persist durable MySQL Provider outcome marker: %v", err)
	}
	if err := db.Model(&diagnosis.DiagnosisAttempt{}).Where("id = ?", attempt.ID).Updates(map[string]interface{}{
		"status": diagnosis.AttemptStatusAbandoned, "error_code": "LEASE_EXPIRED",
	}).Error; err != nil {
		t.Fatalf("simulate attempt cleanup before job reaping: %v", err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatalf("expire MySQL Job lease: %v", err)
	}

	reaped, err := jobStore.ReapExpiredJobs(ctx, 10)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredJobs = %d, err=%v; want one reaped Diagnosis Job", reaped, err)
	}
	savedJob, err := jobStore.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	savedRun, err := diagnosisStore.GetByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := diagnosisStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if savedJob.Status != jobs.StatusFailed || savedJob.LastErrorClass == nil || *savedJob.LastErrorClass != string(jobs.ErrorClassPermanent) ||
		savedJob.LastErrorCode == nil || *savedJob.LastErrorCode != llm.OutcomeUnknownErrorCode ||
		savedJob.TerminalReason == nil || *savedJob.TerminalReason != jobs.TerminalReasonPermanent {
		t.Fatalf("reaped MySQL Job=%+v; want FAILED/PERMANENT/PROVIDER_OUTCOME_UNKNOWN", savedJob)
	}
	if savedRun.Status != diagnosis.StatusFailed || len(attempts) != 1 || attempts[0].Status != diagnosis.AttemptStatusFailedTerminal ||
		attempts[0].ErrorCode != llm.OutcomeUnknownErrorCode || attempts[0].ProviderCalls != 1 {
		t.Fatalf("reaped MySQL Diagnosis state: run=%+v attempts=%+v", savedRun, attempts)
	}
}

func f01OptionalString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func f01OptionalTerminalReason(value *jobs.TerminalReason) string {
	if value == nil {
		return ""
	}
	return string(*value)
}

type f01ExecutionEvent struct {
	workerShutdown bool
	outcomeUnknown bool
	err            string
}

type f01ObservedExecutor struct {
	delegate agent.Executor
	events   chan f01ExecutionEvent
}

func (e *f01ObservedExecutor) Execute(ctx context.Context, spec diagnosis.DiagnosisExecutionSpec, attempt *diagnosis.DiagnosisAttempt) (*agent.ExecutionResult, error) {
	result, err := e.delegate.Execute(ctx, spec, attempt)
	var unknown *llm.OutcomeUnknownError
	e.events <- f01ExecutionEvent{
		workerShutdown: errors.Is(context.Cause(ctx), jobs.ErrWorkerShutdown),
		outcomeUnknown: errors.As(err, &unknown),
		err:            fmt.Sprint(err),
	}
	return result, err
}

func f01DiagnosisRun(baseURL string) *diagnosis.DiagnosisRun {
	guard := agent.DefaultGuardConfig()
	const providerRetries = 0
	run := &diagnosis.DiagnosisRun{
		ID:                          "f01-shutdown-lease-replay",
		UserID:                      "f01-user",
		RepositoryID:                "f01-repository",
		SnapshotID:                  "f01-snapshot",
		IssueTitle:                  "Provider request outcome is ambiguous during Worker shutdown",
		ProviderEndpointFingerprint: "f01-endpoint-fingerprint",
		ProviderConfigFingerprint:   "f01-config-fingerprint",
		NormalizedBaseURL:           baseURL,
		ModelName:                   "f01-model",
		PromptVersion:               diagnosis.CurrentPromptVersion,
		AgentVersion:                diagnosis.CurrentAgentVersion,
		MaxAgentRounds:              guard.MaxSteps,
		MaxToolCalls:                guard.MaxToolCalls,
		MaxSearchCalls:              guard.MaxSearchCalls,
		MaxRepeatCalls:              guard.MaxRepeatCalls,
		MaxEvidencePacketBytes:      32 * 1024,
		MaxToolResultBytes:          guard.MaxToolResultBytes,
		FinalizationTurns:           1,
		MaxOutputTokens:             guard.MaxOutputTokens,
		ProviderTimeoutSeconds:      30,
		ProviderRetryAttempts:       providerRetries,
		Temperature:                 0.1,
		ReasoningEffort:             "low",
		IdempotencyKey:              "f01-shutdown-lease-replay-key",
		IdempotencyRequestHash:      "f01-shutdown-lease-replay-request-hash",
	}
	run.AgentConfigHash = diagnosis.ComputeAgentConfigHashWithGenerationOptions(
		run.MaxAgentRounds, run.MaxToolCalls, run.MaxSearchCalls, run.MaxRepeatCalls,
		run.MaxEvidencePacketBytes, run.MaxToolResultBytes, run.FinalizationTurns,
		run.MaxOutputTokens, run.ProviderTimeoutSeconds, run.ProviderRetryAttempts,
		run.Temperature, run.ReasoningEffort, "json_object",
	)
	return run
}

func startF01Worker(store *jobs.Store, workerID string, handler jobs.Handler, lease time.Duration) *jobs.Worker {
	cfg := jobs.DefaultWorkerConfig()
	cfg.WorkerID = workerID
	cfg.Concurrency = 1
	cfg.BatchSize = 1
	cfg.PollInterval = 5 * time.Millisecond
	cfg.LeaseDuration = lease
	cfg.ReapInterval = time.Hour
	cfg.BaseBackoff = 5 * time.Millisecond
	cfg.MaxBackoff = 10 * time.Millisecond
	runtime := jobs.NewWorker(store, cfg)
	runtime.RegisterHandler(jobs.JobTypeRunDiagnosis, handler)
	runtime.Start(context.Background())
	return runtime
}

func stopF01Worker(t *testing.T, runtime *jobs.Worker) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := runtime.StopGracefully(ctx); err != nil {
		t.Errorf("StopGracefully: %v", err)
	}
}

func waitUntilF01LeaseExpires(t *testing.T, store *jobs.Store, jobID int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := store.GetJobByID(context.Background(), jobID)
		if err != nil {
			t.Fatalf("read Job while waiting for lease expiry: %v", err)
		}
		if job.Status != jobs.StatusRunning || job.LeaseUntil == nil {
			t.Fatalf("Job left RUNNING before lease expiry: %+v", job)
		}
		if time.Now().After(*job.LeaseUntil) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Worker #1 lease did not expire after it stopped renewing")
}

func waitUntilF01RetryDue(t *testing.T, store *jobs.Store, jobID int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		job, err := store.GetJobByID(context.Background(), jobID)
		if err != nil {
			t.Fatalf("read Job while waiting for automatic retry: %v", err)
		}
		if job.Status != jobs.StatusRetryWait {
			t.Fatalf("Job left RETRY_WAIT before Worker #2 claim: %s", job.Status)
		}
		if !time.Now().Before(job.NextRunAt) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("lease recovery retry did not become due")
}

func waitForF01TerminalJob(store *jobs.Store, jobID int64, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		job, err := store.GetJobByID(context.Background(), jobID)
		if err == nil && (job.Status == jobs.StatusSucceeded || job.Status == jobs.StatusFailed || job.Status == jobs.StatusCancelled) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	job, err := store.GetJobByID(context.Background(), jobID)
	return fmt.Errorf("Job did not reach terminal state before timeout: job=%+v read_error=%v", job, err)
}
