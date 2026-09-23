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

type checkpointFailingStore struct {
	*diagnosis.GormStore
}

func (s checkpointFailingStore) UpdateAttemptCheckpoint(context.Context, string, string, string, bool, int, int, int, int, int, int, int, int, string, string) error {
	return errors.New("checkpoint storage unavailable")
}

func (s checkpointFailingStore) UpdateAttemptCheckpointWithDraft(context.Context, string, string, string, string, string, string, bool, int, int, int, int, int, int, int, int, string, string) error {
	return errors.New("checkpoint storage unavailable")
}

func (e *checkpointCountingExecutor) Execute(context.Context, *diagnosis.DiagnosisRun, *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	e.calls++
	return nil, errors.New("provider should not be called when a checkpoint exists")
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
	if err := db.Create(&diagnosis.DiagnosisAttempt{
		ID:                    "checkpoint-old",
		DiagnosisRunID:        run.ID,
		AttemptNo:             1,
		WorkerID:              "old-worker",
		Status:                diagnosis.AttemptStatusFailedRetryable,
		StartedAt:             time.Now().UTC(),
		HeartbeatAt:           time.Now().UTC(),
		DeadlineAt:            time.Now().UTC().Add(time.Minute),
		RawOutput:             `{"conclusion_kind":"INSUFFICIENT_EVIDENCE","confirmed_facts":["checkpoint evidence is incomplete"],"limitations":["checkpoint evidence"],"recommended_checks":["collect checkpoint evidence"]}`,
		ParsedReportJSON:      `{"conclusion_kind":"INSUFFICIENT_EVIDENCE","confirmed_facts":["checkpoint evidence is incomplete"],"limitations":["checkpoint evidence"],"recommended_checks":["collect checkpoint evidence"]}`,
		StructuredOutputValid: true,
		ProviderCalls:         1,
	}).Error; err != nil {
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
	if err := db.Create(&diagnosis.DiagnosisAttempt{
		ID: "checkpoint-old-evidence", DiagnosisRunID: run.ID, AttemptNo: 1, WorkerID: "old-worker",
		Status: diagnosis.AttemptStatusFailedRetryable, StartedAt: time.Now().UTC(), HeartbeatAt: time.Now().UTC(),
		DeadlineAt: time.Now().UTC().Add(time.Minute), RawOutput: string(draftJSON), ParsedReportDraftJSON: string(draftJSON),
		CheckpointPromptVersion: diagnosis.CurrentPromptVersion, CheckpointAgentVersion: diagnosis.CurrentAgentVersion,
		StructuredOutputValid: true, ProviderCalls: 1,
	}).Error; err != nil {
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
