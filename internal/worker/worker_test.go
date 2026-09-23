package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/agent"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/worker"
)

type checkpointCountingExecutor struct {
	calls int
}

type callCountingSuccessExecutor struct {
	calls int
}

type partialThenValidExecutor struct {
	calls int
}

type checkpointFailingStore struct {
	*diagnosis.GormStore
}

type successFinalizerFailingStore struct{ *diagnosis.GormStore }

type invalidFinalizerFailingStore struct{ *diagnosis.GormStore }

func (s successFinalizerFailingStore) FinalizeSuccess(context.Context, int64, string, string, string, string, *evidence.Report, []evidence.Citation, int, int, int) error {
	return errors.New("injected finalizer rollback")
}

func (s invalidFinalizerFailingStore) FinalizeInvalidStructuredReport(context.Context, int64, string, string, string, string, *evidence.Report, int, int, int, string, string) error {
	return errors.New("injected invalid finalizer rollback")
}

type invalidReportExecutor struct{}

func (invalidReportExecutor) Execute(context.Context, *diagnosis.DiagnosisRun, *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	return &worker.ExecutionResult{RawOutput: `{"unknown":"secret-test-marker"}`, ParseError: "INVALID_STRUCTURED_REPORT: UNKNOWN_FIELD", StructuredReport: false}, fmt.Errorf("%w: UNKNOWN_FIELD", agent.ErrInvalidStructuredReport)
}

func (s checkpointFailingStore) UpdateAttemptCheckpoint(context.Context, string, diagnosis.AttemptCheckpoint) error {
	return errors.New("checkpoint storage unavailable")
}

func (s checkpointFailingStore) UpdateAttemptCheckpointWithDraft(context.Context, string, diagnosis.AttemptCheckpoint) error {
	return errors.New("checkpoint storage unavailable")
}

func (e *checkpointCountingExecutor) Execute(context.Context, *diagnosis.DiagnosisRun, *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	e.calls++
	return nil, errors.New("provider should not be called when a checkpoint exists")
}

func (e *callCountingSuccessExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	e.calls++
	return invalidEvidenceExecutor{}.Execute(ctx, run, attempt)
}

func (e *partialThenValidExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	e.calls++
	if e.calls == 1 {
		return &worker.ExecutionResult{
			RawOutput: `{"progress":"tool completed"}`, FinishReason: "tool_calls", ToolCalls: 1, ProviderCalls: 1,
		}, jobs.NewRetryableError("PROVIDER_TIMEOUT", "provider timed out after partial progress", nil)
	}
	return invalidEvidenceExecutor{}.Execute(ctx, run, attempt)
}

func setupTestEnvironment(t *testing.T) (*gorm.DB, *jobs.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "worker_test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed getting underlying sql.DB: %v", err)
	}
	jobsStore := jobs.NewStoreWithDriver(sqlDB, "sqlite3")
	return db, jobsStore
}

func TestWorkerJobHandler_ExecutionSuccess(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	citVal := evidence.NewCitationValidator(storeFS)
	fakeExec := worker.NewFakeDiagnosisExecutor()

	handler := worker.NewDiagnosisJobHandler(
		diagStore,
		repStore,
		citStore,
		citVal,
		fakeExec,
	)

	workerCfg := jobs.DefaultWorkerConfig()
	workerCfg.PollInterval = 20 * time.Millisecond
	jobsWorker := jobs.NewWorker(jobsStore, workerCfg)
	jobsWorker.RegisterHandler(jobs.JobTypeRunDiagnosis, handler)

	jobsWorker.Start(ctx)
	defer jobsWorker.Stop()

	// Create a diagnosis run (atomically creates analysis_job)
	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-100",
		RepositoryID:           "repo-100",
		SnapshotID:             "snap-100",
		IssueTitle:             "Happy Path Bug",
		IdempotencyKey:         "k-happy",
		IdempotencyRequestHash: "h-happy",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatalf("failed creating diagnosis run: %v", err)
	}

	// Wait for job execution
	deadline := time.Now().Add(5 * time.Second)
	var finalRun *diagnosis.DiagnosisRun
	for time.Now().Before(deadline) {
		r, err := diagStore.GetByID(ctx, run.ID)
		if err == nil && r.Status == diagnosis.StatusSucceeded {
			finalRun = r
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalRun == nil {
		t.Fatalf("diagnosis job did not complete with SUCCEEDED within timeout")
	}

	report, err := repStore.GetByRunID(ctx, run.ID)
	if err != nil || report == nil {
		t.Fatalf("expected report generated, got err: %v", err)
	}
	if report.RootCause == "" {
		t.Errorf("expected non-empty root cause in report")
	}

	// Check AnalysisJob status
	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatalf("failed to fetch analysis job: %v", err)
	}
	if job.Status != jobs.StatusSucceeded {
		t.Errorf("expected job status SUCCEEDED, got %s", job.Status)
	}
}

type invalidEvidenceExecutor struct{}

func (invalidEvidenceExecutor) Execute(context.Context, *diagnosis.DiagnosisRun, *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	return &worker.ExecutionResult{
		Report: &evidence.DiagnosisReportData{
			ConclusionKind: evidence.ConclusionRootCause,
			Summary:        "summary",
			RootCause:      "root cause",
			Findings: []evidence.Finding{{
				Title: "finding", Reasoning: "reasoning",
				Citations: []evidence.Citation{{
					EvidenceID: "ev_fake", ValidationStatus: evidence.CitationInvalid, ValidationError: "EVIDENCE_NOT_FOUND",
				}},
			}},
		},
		StructuredReport: true,
	}, nil
}

func TestWorkerJobHandler_InvalidEvidenceDegradesButSucceeds(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-invalid-evidence", UserID: "user-invalid-evidence", RepositoryID: "repo-invalid-evidence",
		SnapshotID: "snap-invalid-evidence", IssueTitle: "invalid evidence", IdempotencyKey: "invalid-evidence-key", IdempotencyRequestHash: "invalid-evidence-hash",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "worker-invalid-evidence", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed to claim invalid-evidence job: err=%v jobs=%d", err, len(claimed))
	}
	handler := worker.NewDiagnosisJobHandler(diagStore, repStore, citStore, nil, invalidEvidenceExecutor{})
	if err := handler.Execute(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	savedRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusSucceeded {
		t.Fatalf("run = %+v err=%v, want SUCCEEDED", savedRun, err)
	}
	report, err := repStore.GetByRunID(ctx, run.ID)
	if err != nil || report.ReportStatus != evidence.ReportDegraded || report.InvalidCitationCount != 1 {
		t.Fatalf("report = %+v err=%v, want DEGRADED with one invalid citation", report, err)
	}
}

func TestFinalizerRollbackClosesDurableCheckpointAttempt(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	baseStore := diagnosis.NewStore(db)
	reportStore := evidence.NewReportStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-finalizer-rollback", UserID: "user-finalizer-rollback", RepositoryID: "repo-finalizer-rollback",
		SnapshotID: "snap-finalizer-rollback", IssueTitle: "finalizer rollback", IdempotencyKey: "finalizer-rollback-key", IdempotencyRequestHash: "finalizer-rollback-hash",
	}
	if err := baseStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "worker-finalizer-rollback", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim failed: %v", err)
	}
	store := successFinalizerFailingStore{GormStore: baseStore}
	handler := worker.NewDiagnosisJobHandler(store, reportStore, evidence.NewCitationStore(db), nil, invalidEvidenceExecutor{})
	handlerErr := handler.Execute(ctx, claimed[0])
	if handlerErr == nil {
		t.Fatal("expected injected finalizer error")
	}
	attempts, err := baseStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts=%+v err=%v", attempts, err)
	}
	if attempts[0].Status != diagnosis.AttemptStatusFailedRetryable || attempts[0].CheckpointKind != diagnosis.CheckpointKindFinalValid || attempts[0].ProviderCompletedAt == nil {
		t.Fatalf("attempt after finalizer rollback = %+v; want closed retryable final checkpoint", attempts[0])
	}
	savedRun, err := baseStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusRunning {
		t.Fatalf("run after finalizer rollback = %+v err=%v", savedRun, err)
	}
	class, code := jobs.ClassifyError(handlerErr)
	if err := jobsStore.ConditionalFinalizeFailure(ctx, claimed[0].ID, *claimed[0].WorkerID, *claimed[0].ClaimToken, class, code, "finalizer failed", nil, false, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	savedJob, err := jobsStore.GetJobByID(ctx, claimed[0].ID)
	if err != nil || savedJob.Status != jobs.StatusRetryWait {
		t.Fatalf("job after retryable finalizer error = %+v err=%v", savedJob, err)
	}
	retryClaim, err := jobsStore.ClaimJobs(ctx, "worker-finalizer-retry", 1, time.Minute)
	if err != nil || len(retryClaim) != 1 {
		t.Fatalf("same-generation checkpoint retry claim failed: jobs=%d err=%v", len(retryClaim), err)
	}
	executor := &callCountingSuccessExecutor{}
	retryHandler := worker.NewDiagnosisJobHandler(baseStore, reportStore, evidence.NewCitationStore(db), nil, executor)
	if err := retryHandler.Execute(ctx, retryClaim[0]); err != nil {
		t.Fatalf("restore/finalize retry failed: %v", err)
	}
	if executor.calls != 0 {
		t.Fatalf("provider was called %d times on durable checkpoint retry", executor.calls)
	}
	attempts, err = baseStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.Status == diagnosis.AttemptStatusRunning {
			t.Fatalf("historical RUNNING attempt remained: %+v", attempt)
		}
	}
}

func TestInvalidFinalReportCheckpointSurvivesFinalizerRollback(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	baseStore := diagnosis.NewStore(db)
	reportStore := evidence.NewReportStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-invalid-finalizer-rollback", UserID: "user-invalid-finalizer-rollback", RepositoryID: "repo-invalid-finalizer-rollback",
		SnapshotID: "snap-invalid-finalizer-rollback", IssueTitle: "invalid finalizer rollback", IdempotencyKey: "invalid-finalizer-key", IdempotencyRequestHash: "invalid-finalizer-hash",
	}
	if err := baseStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "worker-invalid-finalizer", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim failed: %v", err)
	}
	first := worker.NewDiagnosisJobHandler(invalidFinalizerFailingStore{GormStore: baseStore}, reportStore, evidence.NewCitationStore(db), nil, invalidReportExecutor{})
	if err := first.Execute(ctx, claimed[0]); err == nil {
		t.Fatal("expected invalid finalizer rollback")
	}
	attempts, err := baseStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 1 || attempts[0].CheckpointKind != diagnosis.CheckpointKindFinalInvalid || attempts[0].Status != diagnosis.AttemptStatusFailedRetryable {
		t.Fatalf("invalid checkpoint attempt = %+v err=%v", attempts, err)
	}
	class, code := jobs.ClassifyError(jobs.NewRetryableError("ATOMIC_INVALID_FINALIZE_FAILED", "injected", nil))
	if err := jobsStore.ConditionalFinalizeFailure(ctx, claimed[0].ID, *claimed[0].WorkerID, *claimed[0].ClaimToken, class, code, "retry finalizer", nil, false, time.Now().UTC().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	retryClaim, err := jobsStore.ClaimJobs(ctx, "worker-invalid-finalizer-retry", 1, time.Minute)
	if err != nil || len(retryClaim) != 1 {
		t.Fatalf("retry claim failed: %v", err)
	}
	executor := &checkpointCountingExecutor{}
	retry := worker.NewDiagnosisJobHandler(baseStore, reportStore, evidence.NewCitationStore(db), nil, executor)
	retryErr := retry.Execute(ctx, retryClaim[0])
	if retryErr == nil || !strings.Contains(retryErr.Error(), "INVALID_STRUCTURED_REPORT") {
		t.Fatalf("restored invalid report error = %v", retryErr)
	}
	if executor.calls != 0 {
		t.Fatalf("provider calls after invalid checkpoint restore = %d", executor.calls)
	}
	savedRun, err := baseStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusFailed {
		t.Fatalf("run = %+v err=%v, want FAILED", savedRun, err)
	}
	report, err := reportStore.GetByRunID(ctx, run.ID)
	if err != nil || report.ReportStatus != evidence.ReportInvalid || !strings.Contains(report.ParseError, "UNKNOWN_FIELD") || strings.Contains(report.ParseError, "secret-test-marker") {
		t.Fatalf("invalid report = %+v err=%v", report, err)
	}
	attempts, err = baseStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range attempts {
		if attempt.Status == diagnosis.AttemptStatusRunning {
			t.Fatalf("RUNNING historical attempt: %+v", attempt)
		}
		if strings.Contains(attempt.ErrorMessage, "secret-test-marker") {
			t.Fatalf("untrusted marker reached attempt error metadata: %+v", attempt)
		}
	}
	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil || job.Status != jobs.StatusFailed || job.LastErrorMessage == nil || strings.Contains(*job.LastErrorMessage, "secret-test-marker") {
		t.Fatalf("stable job error metadata = %+v err=%v", job, err)
	}
}

type invalidStructuredReportExecutor struct{}

func (invalidStructuredReportExecutor) Execute(context.Context, *diagnosis.DiagnosisRun, *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	return &worker.ExecutionResult{
		RawOutput:        "{\"conclusion_kind\":\"ROOT_CAUSE\"}",
		ParseError:       "INVALID_STRUCTURED_REPORT: root cause report needs summary, root_cause, and at least one finding",
		StructuredReport: false,
	}, fmt.Errorf("%w: root cause report needs summary, root_cause, and at least one finding", agent.ErrInvalidStructuredReport)
}

func TestWorkerJobHandler_InvalidStructuredReportFailsTerminally(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-invalid-structured", UserID: "user-invalid-structured", RepositoryID: "repo-invalid-structured",
		SnapshotID: "snap-invalid-structured", IssueTitle: "invalid structured", IdempotencyKey: "invalid-structured-key", IdempotencyRequestHash: "invalid-structured-hash",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "worker-invalid-structured", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed to claim invalid-structured job: err=%v jobs=%d", err, len(claimed))
	}
	handler := worker.NewDiagnosisJobHandler(diagStore, repStore, citStore, nil, invalidStructuredReportExecutor{})
	err = handler.Execute(ctx, claimed[0])
	if err == nil || !strings.Contains(err.Error(), "INVALID_STRUCTURED_REPORT") {
		t.Fatalf("expected terminal invalid structured error, got %v", err)
	}
	savedRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusFailed {
		t.Fatalf("run = %+v err=%v, want FAILED", savedRun, err)
	}
	attempts, err := diagStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != diagnosis.AttemptStatusFailedTerminal || attempts[0].ErrorCode != "INVALID_STRUCTURED_REPORT" {
		t.Fatalf("attempts = %+v err=%v, want FAILED_TERMINAL with stable error code", attempts, err)
	}
	report, err := repStore.GetByRunID(ctx, run.ID)
	if err != nil || report == nil || report.ReportStatus != evidence.ReportInvalid || report.RawOutput == "" || !strings.Contains(report.ParseError, "INVALID_STRUCTURED_REPORT") {
		t.Fatalf("report = %+v err=%v, want INVALID report with raw output and parse error", report, err)
	}
	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil || job.Status != jobs.StatusFailed || job.TerminalReason == nil || *job.TerminalReason != jobs.TerminalReasonPermanent {
		t.Fatalf("job = %+v err=%v, want FAILED/PERMANENT", job, err)
	}
}

type cancellingDiagnosisExecutor struct {
	cancel context.CancelFunc
}

func (e cancellingDiagnosisExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	e.cancel()
	return nil, context.Canceled
}

func TestWorkerJobHandler_CancellationFinalizesWithIndependentContext(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	citVal := evidence.NewCitationValidator(storeFS)

	run := &diagnosis.DiagnosisRun{
		ID:                     "run-cancel-execution-context",
		UserID:                 "user-cancel",
		RepositoryID:           "repo-cancel",
		SnapshotID:             "snap-cancel",
		IssueTitle:             "Cancellation context test",
		IdempotencyKey:         "cancel-execution-context",
		IdempotencyRequestHash: "cancel-execution-context-hash",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "worker-cancel", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed to claim diagnosis job: err=%v jobs=%d", err, len(claimed))
	}

	handler := worker.NewDiagnosisJobHandler(
		diagStore,
		repStore,
		citStore,
		citVal,
		cancellingDiagnosisExecutor{cancel: cancel},
	)
	err = handler.Execute(ctx, claimed[0])
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation error, got %v", err)
	}

	savedRun, err := diagStore.GetByID(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if savedRun.Status != diagnosis.StatusCancelled {
		t.Fatalf("diagnosis status = %s, want CANCELLED", savedRun.Status)
	}
	attempts, err := diagStore.ListAttemptsByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 1 || attempts[0].Status != diagnosis.AttemptStatusCancelled {
		t.Fatalf("attempts = %+v, want one CANCELLED attempt", attempts)
	}
	job, err := jobsStore.GetJobByResource(context.Background(), jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusCancelled {
		t.Fatalf("job status = %s, want CANCELLED", job.Status)
	}
}

func TestWorkerJobHandlerResumesFromProviderCheckpoint(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	citVal := evidence.NewCitationValidator(storeFS)
	run := &diagnosis.DiagnosisRun{
		ID:                     "run-checkpoint-resume",
		UserID:                 "user-checkpoint",
		RepositoryID:           "repo-checkpoint",
		SnapshotID:             "snap-checkpoint",
		IssueTitle:             "checkpoint",
		IdempotencyKey:         "checkpoint-key",
		IdempotencyRequestHash: "checkpoint-hash",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	checkpointCompletedAt := time.Now().UTC()
	if err := db.Create(&diagnosis.DiagnosisAttempt{
		ID:                    "checkpoint-old",
		DiagnosisRunID:        run.ID,
		ExecutionGeneration:   1,
		AttemptNo:             1,
		WorkerID:              "old-worker",
		Status:                diagnosis.AttemptStatusFailedRetryable,
		StartedAt:             time.Now().UTC(),
		HeartbeatAt:           time.Now().UTC(),
		DeadlineAt:            time.Now().UTC().Add(time.Minute),
		RawOutput:             `{"conclusion_kind":"INSUFFICIENT_EVIDENCE","confirmed_facts":["checkpoint evidence is incomplete"],"limitations":["checkpoint evidence"],"recommended_checks":["collect checkpoint evidence"]}`,
		ParsedReportJSON:      `{"conclusion_kind":"INSUFFICIENT_EVIDENCE","confirmed_facts":["checkpoint evidence is incomplete"],"limitations":["checkpoint evidence"],"recommended_checks":["collect checkpoint evidence"]}`,
		StructuredOutputValid: true,
		CheckpointKind:        diagnosis.CheckpointKindFinalValid,
		ProviderCompletedAt:   &checkpointCompletedAt,
		ProviderCalls:         1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("resource_id = ?", run.ID).Update("attempt_count", 1).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "worker-checkpoint", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed to claim checkpoint job: err=%v jobs=%d", err, len(claimed))
	}
	executor := &checkpointCountingExecutor{}
	handler := worker.NewDiagnosisJobHandler(diagStore, repStore, citStore, citVal, executor)
	if err := handler.Execute(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want checkpoint resume without provider", executor.calls)
	}
	savedRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusSucceeded {
		t.Fatalf("saved run = %+v err=%v, want SUCCEEDED", savedRun, err)
	}
	report, err := repStore.GetByRunID(ctx, run.ID)
	if err != nil || report.ReportStatus != evidence.ReportInsufficientEvidence {
		t.Fatalf("checkpoint report = %+v err=%v", report, err)
	}
}

func TestExplicitRetryUsesNewAttemptGenerationAndIgnoresOldCheckpoint(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-explicit-generation-retry", UserID: "user-explicit-generation-retry", RepositoryID: "repo-generation",
		SnapshotID: "snap-generation", IssueTitle: "generation retry", IdempotencyKey: "generation-retry-key", IdempotencyRequestHash: "generation-retry-hash",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for n := 1; n <= 3; n++ {
		checkpointKind := diagnosis.CheckpointKindNone
		output := ""
		parsedReport := ""
		structured := false
		if n == 3 {
			checkpointKind = diagnosis.CheckpointKindFinalValid
			output = `{"summary":"old generation result"}`
			parsedReport = `{"conclusion_kind":"ROOT_CAUSE","summary":"old generation result","root_cause":"old","findings":[{"title":"old","reasoning":"old"}]}`
			structured = true
		}
		attempt := diagnosis.DiagnosisAttempt{
			ID: fmt.Sprintf("generation-one-attempt-%d", n), DiagnosisRunID: run.ID, ExecutionGeneration: 1, AttemptNo: n,
			WorkerID: "previous-worker", Status: diagnosis.AttemptStatusFailedRetryable, StartedAt: now,
			HeartbeatAt: now, DeadlineAt: now.Add(time.Minute), RawOutput: output, ParsedReportJSON: parsedReport,
			StructuredOutputValid: structured, CheckpointKind: checkpointKind,
		}
		if checkpointKind == diagnosis.CheckpointKindFinalValid {
			attempt.ProviderCompletedAt = &now
		}
		if err := db.Create(&attempt).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Model(&diagnosis.DiagnosisRun{}).Where("id = ?", run.ID).Update("status", diagnosis.StatusFailed).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("resource_id = ?", run.ID).Updates(map[string]interface{}{
		"status": jobs.StatusFailed, "attempt_count": 3, "last_error_code": "PROVIDER_TIMEOUT",
		"last_error_class": jobs.ErrorClassRetryable,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := jobsStore.RetryDiagnosis(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "generation-two-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed to claim explicit retry: err=%v jobs=%d", err, len(claimed))
	}
	if claimed[0].ExecutionGeneration != 2 || claimed[0].AttemptCount != 1 {
		t.Fatalf("claimed retry = generation %d attempt %d, want generation 2 attempt 1", claimed[0].ExecutionGeneration, claimed[0].AttemptCount)
	}
	executor := &callCountingSuccessExecutor{}
	handler := worker.NewDiagnosisJobHandler(diagStore, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil, executor)
	if err := handler.Execute(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	if executor.calls != 1 {
		t.Fatalf("provider executions = %d, want one fresh call in generation 2", executor.calls)
	}
	attempts, err := diagStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 4 {
		t.Fatalf("attempt count = %d, want 4: %+v", len(attempts), attempts)
	}
	seenIDs := make(map[string]bool, len(attempts))
	for i, attempt := range attempts {
		if seenIDs[attempt.ID] {
			t.Fatalf("attempt ID %q was reused", attempt.ID)
		}
		seenIDs[attempt.ID] = true
		if i < 3 && (attempt.ExecutionGeneration != 1 || attempt.AttemptNo != i+1) {
			t.Fatalf("old attempt order[%d] = generation %d attempt %d", i, attempt.ExecutionGeneration, attempt.AttemptNo)
		}
	}
	current := attempts[len(attempts)-1]
	if current.ExecutionGeneration != 2 || current.AttemptNo != 1 || current.Status != diagnosis.AttemptStatusSucceeded {
		t.Fatalf("current attempt = %+v, want generation 2 attempt 1 SUCCEEDED", current)
	}
	checkpoint, err := diagStore.GetLatestFinalCheckpoint(ctx, run.ID, 2)
	if err != nil || checkpoint == nil || checkpoint.ID != current.ID {
		t.Fatalf("generation 2 checkpoint = %+v err=%v, want current attempt", checkpoint, err)
	}
}

func TestWorkerJobHandlerResumesFinalInvalidCheckpointWithoutProvider(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-final-invalid-checkpoint", UserID: "user-final-invalid-checkpoint", RepositoryID: "repo-invalid-checkpoint",
		SnapshotID: "snap-invalid-checkpoint", IssueTitle: "invalid checkpoint", IdempotencyKey: "invalid-checkpoint-key", IdempotencyRequestHash: "invalid-checkpoint-hash",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.Create(&diagnosis.DiagnosisAttempt{
		ID: "old-invalid-attempt", DiagnosisRunID: run.ID, ExecutionGeneration: 1, AttemptNo: 1, WorkerID: "previous-worker",
		Status: diagnosis.AttemptStatusFailedRetryable, StartedAt: now, HeartbeatAt: now, DeadlineAt: now.Add(time.Minute),
		RawOutput: "not-json", CheckpointKind: diagnosis.CheckpointKindFinalInvalid,
		CheckpointErrorCode: "INVALID_STRUCTURED_REPORT", CheckpointErrorMessage: "agent returned an invalid structured report",
		ProviderCompletedAt: &now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("resource_id = ?", run.ID).Update("attempt_count", 1).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "invalid-checkpoint-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed to claim invalid checkpoint retry: err=%v jobs=%d", err, len(claimed))
	}
	executor := &checkpointCountingExecutor{}
	handler := worker.NewDiagnosisJobHandler(diagStore, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil, executor)
	if err := handler.Execute(ctx, claimed[0]); err == nil {
		t.Fatal("expected the restored invalid report to terminalize with an error")
	}
	if executor.calls != 0 {
		t.Fatalf("provider executions = %d, want zero for FINAL_INVALID recovery", executor.calls)
	}
	savedRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusFailed {
		t.Fatalf("run = %+v err=%v, want FAILED", savedRun, err)
	}
	report, err := evidence.NewReportStore(db).GetByRunID(ctx, run.ID)
	if err != nil || report.ReportStatus != evidence.ReportInvalid {
		t.Fatalf("report = %+v err=%v, want INVALID", report, err)
	}
	attempts, err := diagStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 2 || attempts[1].ExecutionGeneration != 1 || attempts[1].AttemptNo != 2 || attempts[1].Status != diagnosis.AttemptStatusFailedTerminal {
		t.Fatalf("attempts = %+v err=%v, want current generation attempt 2 terminal", attempts, err)
	}
}

func TestWorkerJobHandlerDoesNotReplayPartialCheckpoint(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-partial-checkpoint", UserID: "user-partial-checkpoint", RepositoryID: "repo-partial-checkpoint",
		SnapshotID: "snap-partial-checkpoint", IssueTitle: "partial checkpoint", IdempotencyKey: "partial-checkpoint-key", IdempotencyRequestHash: "partial-checkpoint-hash",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "partial-checkpoint-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed to claim first provider attempt: err=%v jobs=%d", err, len(claimed))
	}
	executor := &partialThenValidExecutor{}
	handler := worker.NewDiagnosisJobHandler(diagStore, evidence.NewReportStore(db), evidence.NewCitationStore(db), nil, executor)
	if err := handler.Execute(ctx, claimed[0]); err == nil {
		t.Fatal("expected first provider attempt to fail")
	}
	if err := jobsStore.ConditionalFinalizeFailure(ctx, claimed[0].ID, *claimed[0].WorkerID, *claimed[0].ClaimToken,
		jobs.ErrorClassRetryable, "PROVIDER_TIMEOUT", "provider timed out", nil, false, time.Now().UTC().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	var partial diagnosis.DiagnosisAttempt
	if err := db.Where("diagnosis_run_id = ? AND attempt_no = ?", run.ID, 1).First(&partial).Error; err != nil {
		t.Fatal(err)
	}
	if partial.CheckpointKind != diagnosis.CheckpointKindPartialProviderFailure || partial.ProviderCompletedAt != nil {
		t.Fatalf("partial checkpoint = %+v, want typed partial without provider completion", partial)
	}
	claimed, err = jobsStore.ClaimJobs(ctx, "partial-checkpoint-worker", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		job, jobErr := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
		t.Fatalf("failed to claim automatic retry: err=%v jobs=%d job=%+v jobErr=%v", err, len(claimed), job, jobErr)
	}
	if claimed[0].ExecutionGeneration != 1 || claimed[0].AttemptCount != 2 {
		t.Fatalf("automatic retry = generation %d attempt %d, want generation 1 attempt 2", claimed[0].ExecutionGeneration, claimed[0].AttemptCount)
	}
	if err := handler.Execute(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	if executor.calls != 2 {
		t.Fatalf("provider executions = %d, want fresh provider call after partial checkpoint", executor.calls)
	}
	checkpoint, err := diagStore.GetLatestFinalCheckpoint(ctx, run.ID, 1)
	if err != nil || checkpoint == nil || checkpoint.AttemptNo != 2 {
		t.Fatalf("latest final checkpoint = %+v err=%v, want attempt 2", checkpoint, err)
	}
}

func TestWorkerJobHandlerRebindsCheckpointEvidenceToNewAttempt(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	root, err := storeFS.EnsureDir("repo-checkpoint-evidence", "snap-checkpoint-evidence")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	evidenceIssuer := evidence.NewEvidenceIssuerWithStore(storeFS, evidence.NewEvidenceStore(db))
	run := &diagnosis.DiagnosisRun{
		ID: "run-checkpoint-evidence", UserID: "user-checkpoint-evidence", RepositoryID: "repo-checkpoint-evidence",
		SnapshotID: "snap-checkpoint-evidence", CodeIndexBuildID: 1, IssueTitle: "checkpoint evidence",
		IdempotencyKey: "checkpoint-evidence-key", IdempotencyRequestHash: "checkpoint-evidence-hash",
		PromptVersion: diagnosis.CurrentPromptVersion, AgentVersion: diagnosis.CurrentAgentVersion,
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	oldItem, err := evidenceIssuer.Issue(ctx, evidence.IssueRequest{
		AttemptID: "checkpoint-old-evidence", DiagnosisRunID: run.ID, RepositoryID: run.RepositoryID,
		SnapshotID: run.SnapshotID, CodeIndexBuildID: run.CodeIndexBuildID, SourceKind: evidence.SourceReadFile,
		FilePath: "main.go", StartLine: 1, EndLine: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	draft := evidence.ReportDraft{
		ConclusionKind: evidence.ConclusionRootCause, Summary: "summary", RootCause: "root cause",
		Findings: []evidence.FindingDraft{{Title: "finding", Reasoning: "reasoning", Citations: []evidence.CitationRef{{EvidenceID: oldItem.ID, Reason: "supports"}}}},
	}
	draftJSON, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	checkpointCompletedAt := time.Now().UTC()
	if err := db.Create(&diagnosis.DiagnosisAttempt{
		ID: "checkpoint-old-evidence", DiagnosisRunID: run.ID, ExecutionGeneration: 1, AttemptNo: 1, WorkerID: "old-worker",
		Status: diagnosis.AttemptStatusFailedRetryable, StartedAt: checkpointCompletedAt, HeartbeatAt: checkpointCompletedAt,
		DeadlineAt: checkpointCompletedAt.Add(time.Minute), RawOutput: string(draftJSON), ParsedReportDraftJSON: string(draftJSON),
		CheckpointPromptVersion: diagnosis.CurrentPromptVersion, CheckpointAgentVersion: diagnosis.CurrentAgentVersion,
		StructuredOutputValid: true, CheckpointKind: diagnosis.CheckpointKindFinalValid, ProviderCalls: 1,
		ProviderCompletedAt: &checkpointCompletedAt,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&jobs.AnalysisJob{}).Where("resource_id = ?", run.ID).Update("attempt_count", 1).Error; err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "worker-checkpoint-evidence", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed to claim checkpoint job: err=%v jobs=%d", err, len(claimed))
	}
	executor := &checkpointCountingExecutor{}
	handler := worker.NewDiagnosisJobHandler(diagStore, repStore, citStore, nil, executor).WithEvidenceIssuer(evidenceIssuer)
	if err := handler.Execute(ctx, claimed[0]); err != nil {
		t.Fatal(err)
	}
	if executor.calls != 0 {
		t.Fatalf("executor calls = %d, want checkpoint resume without provider", executor.calls)
	}
	report, err := repStore.GetByRunID(ctx, run.ID)
	if err != nil || report.ReportStatus != evidence.ReportValid {
		t.Fatalf("checkpoint report = %+v err=%v, want VALID", report, err)
	}
	citations, err := citStore.ListByReportID(ctx, report.ID)
	if err != nil || len(citations) != 1 || citations[0].ValidationStatus != evidence.CitationValid {
		t.Fatalf("checkpoint citations = %+v err=%v, want one VALID citation", citations, err)
	}
	if citations[0].EvidenceID == oldItem.ID {
		t.Fatal("checkpoint reused an evidence ID from the old attempt")
	}
	attempts, err := diagStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var currentAttemptID string
	for _, attempt := range attempts {
		if attempt.ID != oldItem.AttemptID && attempt.Status == diagnosis.AttemptStatusSucceeded {
			currentAttemptID = attempt.ID
		}
	}
	if currentAttemptID == "" {
		t.Fatalf("no succeeded replacement attempt: %+v", attempts)
	}
	issued, err := evidenceIssuer.ListByAttempt(ctx, currentAttemptID)
	if err != nil || len(issued) != 1 || issued[0].ID != citations[0].EvidenceID {
		t.Fatalf("replacement evidence = %+v err=%v, want citation-scoped item", issued, err)
	}
}

func TestWorkerJobHandlerCheckpointFailureStopsAutomaticRetry(t *testing.T) {
	db, jobsStore := setupTestEnvironment(t)
	ctx := context.Background()
	baseStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-checkpoint-failure", UserID: "user-checkpoint-failure", RepositoryID: "repo-checkpoint-failure",
		SnapshotID: "snap-checkpoint-failure", IssueTitle: "checkpoint failure", IdempotencyKey: "checkpoint-failure-key", IdempotencyRequestHash: "checkpoint-failure-hash",
	}
	if err := baseStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	claimed, err := jobsStore.ClaimJobs(ctx, "worker-checkpoint-failure", 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("failed to claim checkpoint job: err=%v jobs=%d", err, len(claimed))
	}
	store := checkpointFailingStore{GormStore: baseStore}
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	handler := worker.NewDiagnosisJobHandler(
		store,
		evidence.NewReportStore(db),
		evidence.NewCitationStore(db),
		evidence.NewCitationValidator(storeFS),
		worker.NewFakeDiagnosisExecutor(),
	)
	if err := handler.Execute(ctx, claimed[0]); err == nil {
		t.Fatal("expected checkpoint failure")
	}
	savedRun, err := baseStore.GetByID(ctx, run.ID)
	if err != nil || savedRun.Status != diagnosis.StatusFailed {
		t.Fatalf("run after checkpoint failure = %+v err=%v, want FAILED", savedRun, err)
	}
	attempts, err := baseStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 1 || attempts[0].Status != diagnosis.AttemptStatusFailedTerminal || attempts[0].ErrorCode != "CHECKPOINT_SAVE_FAILED" {
		t.Fatalf("attempts after checkpoint failure = %+v err=%v", attempts, err)
	}
}
