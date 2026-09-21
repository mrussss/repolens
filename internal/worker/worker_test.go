package worker_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

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

func (s checkpointFailingStore) UpdateAttemptCheckpoint(context.Context, string, string, string, bool, int, int, int, int, int, int, int, int, string) error {
	return errors.New("checkpoint storage unavailable")
}

func (s checkpointFailingStore) UpdateAttemptCheckpointWithDraft(context.Context, string, string, string, string, string, string, bool, int, int, int, int, int, int, int, int, string) error {
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
