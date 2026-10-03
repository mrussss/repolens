package worker_test

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"gorm.io/gorm"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	"repolens/internal/worker"
)

type dispatchTestProvider struct {
	calls int
	fn    func(int) (llm.GenerateResponse, error)
}

func (p *dispatchTestProvider) Generate(context.Context, llm.GenerateRequest) (llm.GenerateResponse, error) {
	p.calls++
	return p.fn(p.calls)
}

type diagnosisStoreDispatchGuard struct {
	store   diagnosis.Store
	job     *jobs.AnalysisJob
	runID   string
	attempt *diagnosis.DiagnosisAttempt
}

type dispatchThenRetryableFailureExecutor struct {
	provider *dispatchTestProvider
}

func (e dispatchThenRetryableFailureExecutor) Execute(ctx context.Context, _ diagnosis.DiagnosisExecutionSpec, _ *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	guard := llm.ProviderDispatchGuardFromContext(ctx)
	if guard == nil {
		return nil, errors.New("production execution context has no Provider dispatch guard")
	}
	if _, err := llm.GuardProvider(e.provider, guard).Generate(ctx, llm.GenerateRequest{}); err != nil {
		return nil, err
	}
	return nil, jobs.NewRetryableError("TOOL_FAILURE", "tool failed after provider success", nil)
}

func (g diagnosisStoreDispatchGuard) BeginProviderDispatch(ctx context.Context) error {
	if err := g.store.BeginProviderDispatch(ctx, g.job.ID, *g.job.WorkerID, *g.job.ClaimToken,
		g.job.ExecutionGeneration, g.runID, g.attempt.ID, g.attempt.AttemptNo); err != nil {
		return err
	}
	return g.store.CheckProviderDispatchAuthority(ctx, g.job.ID, *g.job.WorkerID, *g.job.ClaimToken,
		g.job.ExecutionGeneration, g.runID, g.attempt.ID, g.attempt.AttemptNo)
}

func (g diagnosisStoreDispatchGuard) ProviderDispatchSucceeded(ctx context.Context) error {
	return g.store.ProviderDispatchSucceeded(ctx, g.job.ID, *g.job.WorkerID, *g.job.ClaimToken,
		g.job.ExecutionGeneration, g.runID, g.attempt.ID, g.attempt.AttemptNo)
}

func (g diagnosisStoreDispatchGuard) ProviderDispatchFailedDefinitely(ctx context.Context) (bool, error) {
	return g.store.ProviderDispatchFailedDefinitely(ctx, g.job.ID, *g.job.WorkerID, *g.job.ClaimToken,
		g.job.ExecutionGeneration, g.runID, g.attempt.ID, g.attempt.AttemptNo)
}

func createClaimedDiagnosisAttempt(t *testing.T, attemptID, workerID string) (*gorm.DB, *jobs.Store, *diagnosis.GormStore, *jobs.AnalysisJob, *diagnosis.DiagnosisRun, *diagnosis.DiagnosisAttempt) {
	t.Helper()
	db, jobStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "dispatch-" + attemptID, UserID: "dispatch-user", RepositoryID: "dispatch-repo",
		SnapshotID: "dispatch-snapshot", IssueTitle: "dispatch boundary", IdempotencyKey: "dispatch-key-" + attemptID,
		IdempotencyRequestHash: "dispatch-hash-" + attemptID,
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatalf("create Diagnosis: %v", err)
	}
	claimed, err := jobStore.ClaimJobs(ctx, workerID, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim Job: claimed=%d err=%v", len(claimed), err)
	}
	job := claimed[0]
	startClaimedJob(t, jobStore, job, workerID)
	attempt := &diagnosis.DiagnosisAttempt{
		ID: attemptID, DiagnosisRunID: run.ID, ExecutionGeneration: job.ExecutionGeneration,
		AttemptNo: job.AttemptCount, WorkerID: workerID, Status: diagnosis.AttemptStatusRunning,
		StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(), DeadlineAt: time.Now().UTC().Add(time.Hour),
	}
	if err := diagStore.StartAttemptWithClaim(ctx, job.ID, workerID, *job.ClaimToken, job.ExecutionGeneration, run.ID, attempt); err != nil {
		t.Fatalf("StartAttemptWithClaim: %v", err)
	}
	return db, jobStore, diagStore, job, run, attempt
}

func expireAndReapDiagnosisJob(t *testing.T, db *gorm.DB, jobStore *jobs.Store, job *jobs.AnalysisJob) {
	t.Helper()
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatalf("expire Job lease: %v", err)
	}
	reaped, err := jobStore.ReapExpiredJobs(context.Background(), 10)
	if err != nil || reaped != 1 {
		t.Fatalf("ReapExpiredJobs = %d, %v; want 1", reaped, err)
	}
}

func TestStaleStartAttemptWithClaimAfterReaperDoesNotCreateAttempt(t *testing.T) {
	db, jobStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{ID: "dispatch-stale-start", UserID: "u", RepositoryID: "r", SnapshotID: "s", IssueTitle: "stale", IdempotencyKey: "stale-start", IdempotencyRequestHash: "stale-start-hash"}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimJobs(ctx, "dispatch-old-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim Job: jobs=%d err=%v", len(claimed), err)
	}
	job := claimed[0]
	startClaimedJob(t, jobStore, job, "dispatch-old-worker")
	expireAndReapDiagnosisJob(t, db, jobStore, job)
	attempt := &diagnosis.DiagnosisAttempt{
		ID: "dispatch-stale-start-attempt", DiagnosisRunID: run.ID, ExecutionGeneration: job.ExecutionGeneration,
		AttemptNo: job.AttemptCount, WorkerID: "dispatch-old-worker", Status: diagnosis.AttemptStatusRunning,
		StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(), DeadlineAt: time.Now().UTC().Add(time.Hour),
	}
	if err := diagStore.StartAttemptWithClaim(ctx, job.ID, "dispatch-old-worker", *job.ClaimToken, job.ExecutionGeneration, run.ID, attempt); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale StartAttemptWithClaim error = %v, want ErrOwnershipLost", err)
	}
	attempts, err := diagStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("stale worker created attempts=%+v err=%v", attempts, err)
	}
	savedRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusQueued {
		t.Fatalf("run after stale start = %+v err=%v; want QUEUED", savedRun, err)
	}
}

func TestStaleCheckpointAndProviderDispatchRejectedAfterReaper(t *testing.T) {
	db, jobStore, diagStore, job, run, attempt := createClaimedDiagnosisAttempt(t, "dispatch-stale-writers-attempt", "dispatch-old-writer")
	ctx := context.Background()
	if err := diagStore.UpdateAttemptCheckpointWithClaim(ctx, job.ID, *job.WorkerID, *job.ClaimToken,
		job.ExecutionGeneration, attempt.AttemptNo, run.ID, attempt.ID,
		diagnosis.AttemptCheckpoint{ExecutionGeneration: job.ExecutionGeneration, Kind: diagnosis.CheckpointKindFinalValid, RawOutput: "original", ParsedReportJSON: "original report", ParsedDraftJSON: "original draft"}, true); err != nil {
		t.Fatalf("seed initial checkpoint: %v", err)
	}
	expireAndReapDiagnosisJob(t, db, jobStore, job)
	checkpoint := diagnosis.AttemptCheckpoint{
		ExecutionGeneration: job.ExecutionGeneration, Kind: diagnosis.CheckpointKindFinalValid,
		RawOutput: "stale overwrite", ParsedReportJSON: "stale report", ParsedDraftJSON: "stale draft",
	}
	if err := diagStore.UpdateAttemptCheckpointWithClaim(ctx, job.ID, *job.WorkerID, *job.ClaimToken,
		job.ExecutionGeneration, attempt.AttemptNo, run.ID, attempt.ID, checkpoint, true); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale checkpoint error = %v, want ErrOwnershipLost", err)
	}
	provider := &dispatchTestProvider{fn: func(int) (llm.GenerateResponse, error) { return llm.GenerateResponse{}, nil }}
	guard := diagnosisStoreDispatchGuard{store: diagStore, job: job, runID: run.ID, attempt: attempt}
	_, err := llm.GuardProvider(provider, guard).Generate(ctx, llm.GenerateRequest{})
	if !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale guarded Provider error = %v, want ErrOwnershipLost", err)
	}
	if provider.calls != 0 {
		t.Fatalf("stale worker called underlying Provider %d times, want 0", provider.calls)
	}
	savedAttempt, err := diagStore.GetAttempt(ctx, attempt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if savedAttempt.CheckpointKind != diagnosis.CheckpointKindFinalValid || savedAttempt.RawOutput != "original" || savedAttempt.ParsedReportJSON != "original report" || savedAttempt.ParsedReportDraftJSON != "original draft" || savedAttempt.ProviderCompletedAt == nil {
		t.Fatalf("stale checkpoint changed durable attempt fields: %+v", savedAttempt)
	}
}

func TestProviderDispatchMarkerResolvesOnlyAtSafeBoundary(t *testing.T) {
	t.Run("normal success clears on final checkpoint", func(t *testing.T) {
		db, _, store, job, run, attempt := createClaimedDiagnosisAttempt(t, "dispatch-success-attempt", "dispatch-success-worker")
		defer func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() }()
		provider := &dispatchTestProvider{fn: func(int) (llm.GenerateResponse, error) {
			return llm.GenerateResponse{Message: llm.Message{Content: "ok"}}, nil
		}}
		guard := diagnosisStoreDispatchGuard{store: store, job: job, runID: run.ID, attempt: attempt}
		response, err := llm.GuardProvider(provider, guard).Generate(context.Background(), llm.GenerateRequest{})
		if err != nil || response.Message.Content != "ok" || provider.calls != 1 {
			t.Fatalf("guarded success response=%+v err=%v calls=%d", response, err, provider.calls)
		}
		saved, err := store.GetAttempt(context.Background(), attempt.ID)
		if err != nil || !saved.ProviderDispatchUnresolved || !saved.ProviderDispatchSuccessSeen {
			t.Fatalf("after in-memory Provider success marker attempt=%+v err=%v; want unresolved until checkpoint", saved, err)
		}
		if err := store.UpdateAttemptCheckpointWithClaim(context.Background(), job.ID, *job.WorkerID, *job.ClaimToken,
			job.ExecutionGeneration, attempt.AttemptNo, run.ID, attempt.ID,
			diagnosis.AttemptCheckpoint{ExecutionGeneration: job.ExecutionGeneration, Kind: diagnosis.CheckpointKindFinalValid, RawOutput: "saved"}, true); err != nil {
			t.Fatal(err)
		}
		saved, err = store.GetAttempt(context.Background(), attempt.ID)
		if err != nil || saved.ProviderDispatchUnresolved || saved.ProviderDispatchSuccessSeen || saved.ProviderCompletedAt == nil {
			t.Fatalf("after durable final checkpoint attempt=%+v err=%v", saved, err)
		}
	})

	t.Run("definitely pre-dispatch failure resolves marker", func(t *testing.T) {
		db, _, store, job, run, attempt := createClaimedDiagnosisAttempt(t, "dispatch-predispatch-attempt", "dispatch-predispatch-worker")
		defer func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() }()
		provider := &dispatchTestProvider{fn: func(int) (llm.GenerateResponse, error) {
			return llm.GenerateResponse{}, &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
		}}
		guard := diagnosisStoreDispatchGuard{store: store, job: job, runID: run.ID, attempt: attempt}
		_, err := llm.GuardProvider(provider, guard).Generate(context.Background(), llm.GenerateRequest{})
		var outcomeUnknown *llm.OutcomeUnknownError
		if err == nil || errors.As(err, &outcomeUnknown) || provider.calls != 1 {
			t.Fatalf("pre-dispatch result err=%v calls=%d", err, provider.calls)
		}
		saved, getErr := store.GetAttempt(context.Background(), attempt.ID)
		if getErr != nil || saved.ProviderDispatchUnresolved {
			t.Fatalf("pre-dispatch failure did not resolve marker: attempt=%+v err=%v", saved, getErr)
		}
	})

	t.Run("retryable HTTP response remains compatible with explicit retry", func(t *testing.T) {
		db, _, store, job, run, attempt := createClaimedDiagnosisAttempt(t, "dispatch-http-retry-attempt", "dispatch-http-retry-worker")
		defer func() { sqlDB, _ := db.DB(); _ = sqlDB.Close() }()
		provider := &dispatchTestProvider{fn: func(call int) (llm.GenerateResponse, error) {
			if call == 1 {
				return llm.GenerateResponse{}, &llm.HTTPError{StatusCode: 429}
			}
			return llm.GenerateResponse{Message: llm.Message{Content: "ok"}}, nil
		}}
		guard := diagnosisStoreDispatchGuard{store: store, job: job, runID: run.ID, attempt: attempt}
		result, err := llm.GuardProvider(llm.NewRetryingProvider(provider, 1), guard).Generate(context.Background(), llm.GenerateRequest{})
		if err != nil || result.Message.Content != "ok" || provider.calls != 2 {
			t.Fatalf("retryable response flow result=%+v err=%v calls=%d", result, err, provider.calls)
		}
		saved, getErr := store.GetAttempt(context.Background(), attempt.ID)
		if getErr != nil || !saved.ProviderDispatchUnresolved || !saved.ProviderDispatchSuccessSeen {
			t.Fatalf("successful retried request must remain unresolved through final checkpoint: attempt=%+v err=%v", saved, getErr)
		}
	})
}

func TestUncheckpointedProviderSuccessCannotBeRetriedAfterLaterExecutorError(t *testing.T) {
	db, jobStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{ID: "dispatch-later-error", UserID: "u", RepositoryID: "r", SnapshotID: "s", IssueTitle: "later error", IdempotencyKey: "dispatch-later-error-key", IdempotencyRequestHash: "dispatch-later-error-hash"}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobStore.ClaimJobs(ctx, "dispatch-later-error-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim Job: jobs=%d err=%v", len(claimed), err)
	}
	job := claimed[0]
	startClaimedJob(t, jobStore, job, "dispatch-later-error-worker")
	provider := &dispatchTestProvider{fn: func(int) (llm.GenerateResponse, error) {
		return llm.GenerateResponse{Message: llm.Message{Content: "successful provider response"}}, nil
	}}
	handler := worker.NewDiagnosisJobHandler(diagStore, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil,
		dispatchThenRetryableFailureExecutor{provider: provider})
	err = handler.Execute(ctx, job)
	if err == nil {
		t.Fatal("expected the unresolved dispatch to terminalize instead of retrying")
	}
	savedJob, err := jobStore.GetJobByID(ctx, job.ID)
	if err != nil || savedJob.Status != jobs.StatusFailed || savedJob.LastErrorCode == nil || *savedJob.LastErrorCode != llm.OutcomeUnknownErrorCode {
		t.Fatalf("job after post-response executor error=%+v err=%v; want FAILED/PROVIDER_OUTCOME_UNKNOWN", savedJob, err)
	}
	savedRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusFailed {
		t.Fatalf("run after post-response executor error=%+v err=%v", savedRun, err)
	}
	attempts, err := diagStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != diagnosis.AttemptStatusFailedTerminal || attempts[0].ErrorCode != llm.OutcomeUnknownErrorCode || !attempts[0].ProviderDispatchUnresolved {
		t.Fatalf("attempt after post-response executor error=%+v err=%v", attempts, err)
	}
	if provider.calls != 1 {
		t.Fatalf("Provider calls=%d, want exactly 1", provider.calls)
	}
}
