package integration_real

import (
	"context"
	"fmt"
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

type mysqlDispatchGuard struct {
	store   diagnosis.Store
	job     *jobs.AnalysisJob
	runID   string
	attempt *diagnosis.DiagnosisAttempt
}

func (g mysqlDispatchGuard) BeginProviderDispatch(ctx context.Context) error {
	if err := g.store.BeginProviderDispatch(ctx, g.job.ID, *g.job.WorkerID, *g.job.ClaimToken,
		g.job.ExecutionGeneration, g.runID, g.attempt.ID, g.attempt.AttemptNo); err != nil {
		return err
	}
	return g.store.CheckProviderDispatchAuthority(ctx, g.job.ID, *g.job.WorkerID, *g.job.ClaimToken,
		g.job.ExecutionGeneration, g.runID, g.attempt.ID, g.attempt.AttemptNo)
}

func (g mysqlDispatchGuard) ProviderDispatchSucceeded(ctx context.Context) error {
	return g.store.ProviderDispatchSucceeded(ctx, g.job.ID, *g.job.WorkerID, *g.job.ClaimToken,
		g.job.ExecutionGeneration, g.runID, g.attempt.ID, g.attempt.AttemptNo)
}

func (g mysqlDispatchGuard) ProviderDispatchFailedDefinitely(ctx context.Context) (bool, error) {
	return g.store.ProviderDispatchFailedDefinitely(ctx, g.job.ID, *g.job.WorkerID, *g.job.ClaimToken,
		g.job.ExecutionGeneration, g.runID, g.attempt.ID, g.attempt.AttemptNo)
}

func TestRealMySQL_ProviderDispatchCrashDoesNotAutoReplay(t *testing.T) {
	db, jobStore, cleanup := setupRealMySQL(t)
	defer cleanup()
	ctx := context.Background()
	diagnosisStore := diagnosis.NewStore(db)

	var providerCalls atomic.Int32
	providerHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		providerCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, f01SuccessfulProviderReport)
	}))
	defer providerHTTP.Close()

	run := f01DiagnosisRun(providerHTTP.URL)
	run.ID = "f07-provider-dispatch-crash"
	run.IdempotencyKey = "f07-provider-dispatch-crash-key"
	run.IdempotencyRequestHash = "f07-provider-dispatch-crash-hash"
	if err := diagnosisStore.Create(ctx, run); err != nil {
		t.Fatalf("create Diagnosis and Job: %v", err)
	}
	claimed, err := jobStore.ClaimJobs(ctx, "f07-crashed-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim Diagnosis Job: claimed=%+v err=%v", claimed, err)
	}
	job := claimed[0]
	attemptNo, err := jobStore.MarkExecutionStarted(ctx, job.ID, "f07-crashed-worker", *job.ClaimToken, job.ExecutionGeneration)
	if err != nil || attemptNo != 1 {
		t.Fatalf("MarkExecutionStarted: attempt=%d err=%v", attemptNo, err)
	}
	job.AttemptCount = attemptNo
	job.ExecutionStarted = true
	now := time.Now().UTC()
	attempt := &diagnosis.DiagnosisAttempt{
		ID: "f07-crash-attempt", DiagnosisRunID: run.ID, ExecutionGeneration: job.ExecutionGeneration,
		AttemptNo: attemptNo, WorkerID: "f07-crashed-worker", Status: diagnosis.AttemptStatusRunning,
		StartedAt: now, HeartbeatAt: now, DeadlineAt: now.Add(time.Minute),
	}
	if err := diagnosisStore.StartAttemptWithClaim(ctx, job.ID, "f07-crashed-worker", *job.ClaimToken, job.ExecutionGeneration, run.ID, attempt); err != nil {
		t.Fatalf("claim-fenced StartAttempt: %v", err)
	}
	guard := mysqlDispatchGuard{store: diagnosisStore, job: job, runID: run.ID, attempt: attempt}
	provider := llm.GuardProvider(llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout("", providerHTTP.URL, "f07-model", "none", 5*time.Second), guard)
	if _, err := provider.Generate(ctx, llm.GenerateRequest{Model: "f07-model", Messages: []llm.Message{{Role: llm.RoleUser, Content: "dispatch crash test"}}}); err != nil {
		t.Fatalf("first real Provider request: %v", err)
	}
	if providerCalls.Load() != 1 {
		t.Fatalf("initial real HTTP Provider requests=%d, want 1", providerCalls.Load())
	}
	savedAttempt, err := diagnosisStore.GetAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !savedAttempt.ProviderDispatchUnresolved || !savedAttempt.ProviderDispatchSuccessSeen || savedAttempt.CheckpointErrorCode != "" || savedAttempt.CheckpointKind != diagnosis.CheckpointKindNone || savedAttempt.ProviderCompletedAt != nil {
		t.Fatalf("crash-seam state must have only the durable dispatch marker: %+v", savedAttempt)
	}

	// This is the deterministic process-crash seam: the real Provider response
	// and dispatch marker are durable, while no handler checkpoint/finalizer ran.
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatalf("expire crashed Worker lease: %v", err)
	}
	reaped, err := jobStore.ReapExpiredJobs(ctx, 10)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredJobs = %d err=%v; want one terminalized Job", reaped, err)
	}
	savedJob, err := jobStore.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	savedRun, err := diagnosisStore.GetByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	savedAttempts, err := diagnosisStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if savedJob.Status != jobs.StatusFailed || savedJob.TerminalReason == nil || *savedJob.TerminalReason != jobs.TerminalReasonPermanent || savedJob.LastErrorCode == nil || *savedJob.LastErrorCode != llm.OutcomeUnknownErrorCode {
		t.Fatalf("reaped Job=%+v; want FAILED/PERMANENT/PROVIDER_OUTCOME_UNKNOWN", savedJob)
	}
	if savedRun.Status != diagnosis.StatusFailed || savedRun.FinalAttemptID != attempt.ID || len(savedAttempts) != 1 || savedAttempts[0].Status != diagnosis.AttemptStatusFailedTerminal || savedAttempts[0].ErrorCode != llm.OutcomeUnknownErrorCode {
		t.Fatalf("reaped Diagnosis state run=%+v attempts=%+v", savedRun, savedAttempts)
	}

	// A real Worker with the production DiagnosisJobHandler sees no claimable
	// retry and therefore cannot make a second HTTP request.
	diagnosisHandler := worker.NewDiagnosisJobHandler(diagnosisStore, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil,
		agent.NewAgentRuntimeExecutor(llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout("", providerHTTP.URL, "f07-model", "none", 5*time.Second), nil, nil, nil, agent.DefaultGuardConfig()))
	runtime := startF01Worker(jobStore, "f07-after-reaper-worker", diagnosisHandler, time.Second)
	time.Sleep(150 * time.Millisecond)
	stopF01Worker(t, runtime)
	finalJob, err := jobStore.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finalJob.Status != jobs.StatusFailed || providerCalls.Load() != 1 {
		t.Fatalf("post-reaper Worker caused replay: job=%+v provider_calls=%d", finalJob, providerCalls.Load())
	}
}
