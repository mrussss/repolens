package worker_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"repolens/internal/analysispipeline"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

var errValidationFC06AfterCommit = errors.New("validation FC-06 injected error after durable commit")

const validationFC06WorkerID = "validation-fc06-worker"

// VALIDATION-ONLY: convert these observations to desired-invariant regression
// tests during production hardening.
func TestValidationFC06SnapshotAtomicSuccessLeavesWorkerUnresolved(t *testing.T) {
	db, jobStore := setupTestEnvironment(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	if err := repo.NewStore(db).Create(ctx, &repo.Repository{
		ID: "validation-fc06-snapshot-repo", UserID: "validation-fc06-user", Name: "validation-fc06",
		GitURL: "https://example.invalid/validation-fc06", DefaultRef: "main",
	}); err != nil {
		t.Fatal(err)
	}
	prepared, err := analysispipeline.NewService(analysispipeline.NewStore(db)).Prepare(ctx, analysispipeline.PrepareSpec{
		RepositoryID: "validation-fc06-snapshot-repo", SourceRef: "main",
		CommitSHA:       "0123456789012345678901234567890123456789",
		PipelineVersion: revision.PipelineVersion, PipelineFingerprint: revision.ComputePipelineFingerprint(),
		SnapshotBasePath: t.TempDir(), ModulePath: "validation-fc06",
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshotStore := snapshot.NewStore(db)
	materializedPath := t.TempDir()
	handlerReturned := make(chan struct{})
	handler := jobs.HandlerFunc(func(ctx context.Context, job *jobs.AnalysisJob) error {
		err := snapshotStore.FinalizeSnapshotSuccessWithRevision(ctx, job.ID, *job.WorkerID, *job.ClaimToken,
			prepared.SnapshotID, materializedPath, prepared.ID, "validation-fc06", prepared.CommitSHA,
			"validation-fc06-content-hash", 3, 42, time.Now().UTC())
		if err != nil {
			close(handlerReturned)
			return err
		}
		close(handlerReturned)
		return errValidationFC06AfterCommit
	})
	_, stopWorker := startValidationFC06Worker(t, jobStore, jobs.JobTypeMaterializeSnapshot, handler)
	awaitValidationFC06Handler(t, handlerReturned)
	workerErr := stopWorker()

	job, err := jobStore.GetJobByResource(ctx, jobs.JobTypeMaterializeSnapshot, prepared.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	savedSnapshot, err := snapshotStore.GetByID(ctx, prepared.SnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	savedRevision, err := revision.NewStore(db).GetByID(ctx, prepared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != jobs.StatusSucceeded || savedSnapshot.Status != snapshot.StatusReady || savedRevision.Status != revision.StatusPreparing || savedRevision.Stage != revision.StageBuildingCode || savedRevision.CodeIndexBuildID == 0 {
		t.Fatalf("post-commit state: job=%s snapshot=%s revision=%s/%s code_index_build_id=%d",
			job.Status, savedSnapshot.Status, savedRevision.Status, savedRevision.Stage, savedRevision.CodeIndexBuildID)
	}
	nextJob, err := jobStore.GetJobByResource(ctx, jobs.JobTypeBuildCodeIndex, fmt.Sprintf("%d", savedRevision.CodeIndexBuildID))
	if err != nil || nextJob.Status != jobs.StatusPending {
		t.Fatalf("next code-index job=%+v err=%v; want PENDING", nextJob, err)
	}
	if workerErr == nil || !strings.Contains(workerErr.Error(), "unresolved terminal finalization") {
		t.Fatalf("Worker StopGracefully error=%v; want unresolved finalization after successful handler-owned transaction", workerErr)
	}
	t.Logf("observation: durable snapshot/revision/next-job transaction succeeded; current job=%s; Worker returned unresolved error: %v", job.Status, workerErr)
}

// VALIDATION-ONLY: convert this observation to a desired-invariant regression
// test during production hardening.
func TestValidationFC06DiagnosisSuccessLeavesWorkerUnresolved(t *testing.T) {
	db, jobStore := setupTestEnvironment(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	diagnosisStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "validation-fc06-diagnosis-success", UserID: "validation-fc06-user", RepositoryID: "validation-fc06-repo",
		SnapshotID: "validation-fc06-snapshot", IssueTitle: "FC-06 success", IdempotencyKey: "validation-fc06-success-key",
		IdempotencyRequestHash: "validation-fc06-success-hash",
	}
	if err := diagnosisStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}

	handlerReturned := make(chan struct{})
	handler := jobs.HandlerFunc(func(ctx context.Context, job *jobs.AnalysisJob) error {
		attempt := &diagnosis.DiagnosisAttempt{
			ID: uuid.NewString(), DiagnosisRunID: run.ID, ExecutionGeneration: job.ExecutionGeneration,
			AttemptNo: job.AttemptCount, WorkerID: *job.WorkerID,
		}
		if err := diagnosisStore.StartAttempt(ctx, run.ID, attempt); err != nil {
			close(handlerReturned)
			return err
		}
		report := &evidence.Report{
			ID: uuid.NewString(), DiagnosisRunID: run.ID, AttemptID: attempt.ID,
			RootCause: "validation-only committed report", Summary: "FC-06 success evidence",
			ConclusionKind: evidence.ConclusionRootCause, ReportStatus: evidence.ReportValid,
			FindingsJSON: "[]", RecommendedChecksJSON: "[]", StructuredPayloadJSON: "{}", LimitationsJSON: "[]",
		}
		err := diagnosisStore.FinalizeSuccess(ctx, job.ID, *job.WorkerID, *job.ClaimToken, run.ID, attempt.ID, report, nil, 11, 7, 0)
		close(handlerReturned)
		if err != nil {
			return err
		}
		return errValidationFC06AfterCommit
	})
	_, stopWorker := startValidationFC06Worker(t, jobStore, jobs.JobTypeRunDiagnosis, handler)
	awaitValidationFC06Handler(t, handlerReturned)
	workerErr := stopWorker()

	savedRun, err := diagnosisStore.GetByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	savedJob, err := jobStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := diagnosisStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts=%+v err=%v; want one durable attempt", attempts, err)
	}
	report, err := evidence.NewReportStore(db).GetByRunID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if savedRun.Status != diagnosis.StatusSucceeded || savedRun.FinalAttemptID != attempts[0].ID || attempts[0].Status != diagnosis.AttemptStatusSucceeded || savedJob.Status != jobs.StatusSucceeded || report == nil {
		t.Fatalf("post-commit state: run=%s final_attempt=%s attempt=%s job=%s report=%+v",
			savedRun.Status, savedRun.FinalAttemptID, attempts[0].Status, savedJob.Status, report)
	}
	if workerErr == nil || !strings.Contains(workerErr.Error(), "unresolved terminal finalization") {
		t.Fatalf("Worker StopGracefully error=%v; want unresolved finalization after successful diagnosis transaction", workerErr)
	}
	t.Logf("observation: durable diagnosis/report/attempt/job transaction succeeded; Worker returned unresolved error: %v", workerErr)
}

// VALIDATION-ONLY: convert this observation to a desired-invariant regression
// test during production hardening.
func TestValidationFC06DiagnosisCancellationDoesNotLeaveWorkerUnresolved(t *testing.T) {
	db, jobStore := setupTestEnvironment(t)
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	ctx := context.Background()
	diagnosisStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "validation-fc06-diagnosis-cancel", UserID: "validation-fc06-user", RepositoryID: "validation-fc06-repo",
		SnapshotID: "validation-fc06-snapshot", IssueTitle: "FC-06 cancellation", IdempotencyKey: "validation-fc06-cancel-key",
		IdempotencyRequestHash: "validation-fc06-cancel-hash",
	}
	if err := diagnosisStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}

	handlerReturned := make(chan struct{})
	handler := jobs.HandlerFunc(func(ctx context.Context, job *jobs.AnalysisJob) error {
		attempt := &diagnosis.DiagnosisAttempt{
			ID: uuid.NewString(), DiagnosisRunID: run.ID, ExecutionGeneration: job.ExecutionGeneration,
			AttemptNo: job.AttemptCount, WorkerID: *job.WorkerID,
		}
		if err := diagnosisStore.StartAttempt(ctx, run.ID, attempt); err != nil {
			close(handlerReturned)
			return err
		}
		if err := diagnosisStore.RequestCancellation(ctx, run.ID, run.UserID); err != nil {
			close(handlerReturned)
			return err
		}
		err := diagnosisStore.FinalizeCancellation(ctx, job.ID, *job.WorkerID, *job.ClaimToken, run.ID, attempt.ID)
		close(handlerReturned)
		if err != nil {
			return err
		}
		return errValidationFC06AfterCommit
	})
	_, stopWorker := startValidationFC06Worker(t, validationFC06CancelPollStore{Store: jobStore}, jobs.JobTypeRunDiagnosis, handler)
	awaitValidationFC06Handler(t, handlerReturned)
	workerErr := stopWorker()

	savedRun, err := diagnosisStore.GetByID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	savedJob, err := jobStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempts, err := diagnosisStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts=%+v err=%v; want one durable attempt", attempts, err)
	}
	if !savedRun.CancelRequested || savedRun.Status != diagnosis.StatusCancelled || savedRun.FinalAttemptID != attempts[0].ID || attempts[0].Status != diagnosis.AttemptStatusCancelled || savedJob.Status != jobs.StatusCancelled || !savedJob.CancelRequested {
		t.Fatalf("cancellation contract: run=%+v attempt=%+v job=%+v", savedRun, attempts[0], savedJob)
	}
	if workerErr != nil {
		t.Fatalf("Worker StopGracefully error=%v; cancellation reconciliation should accept the durable CANCELLED state", workerErr)
	}
	t.Logf("observation: committed CANCELLED run/attempt/job retained cancel_requested; Worker unresolved finalization=false")
}

type validationFC06WorkerStore interface {
	ClaimJobs(context.Context, string, int, time.Duration) ([]*jobs.AnalysisJob, error)
	GetJobByID(context.Context, int64) (*jobs.AnalysisJob, error)
	MarkExecutionStarted(context.Context, int64, string, string, int) (int, error)
	ReturnUndispatchedClaim(context.Context, int64, string, string, int) error
	RenewLease(context.Context, int64, string, string, time.Time) error
	IsCancelRequested(context.Context, int64, string, string) (bool, error)
	ConditionalFinalizeSuccess(context.Context, int64, string, string) error
	ConditionalFinalizeFailure(context.Context, int64, string, string, jobs.ErrorClass, string, string, *jobs.TerminalReason, bool, time.Time) error
	ConditionalFinalizeCancel(context.Context, int64, string, string) error
	ReapExpiredJobs(context.Context, int) (int, error)
}

type validationFC06CancelPollStore struct{ *jobs.Store }

func (validationFC06CancelPollStore) IsCancelRequested(context.Context, int64, string, string) (bool, error) {
	return false, nil
}

func startValidationFC06Worker(t *testing.T, store validationFC06WorkerStore, jobType jobs.JobType, handler jobs.Handler) (*jobs.Worker, func() error) {
	t.Helper()
	cfg := jobs.DefaultWorkerConfig()
	cfg.WorkerID = validationFC06WorkerID
	cfg.Concurrency = 1
	cfg.BatchSize = 1
	cfg.PollInterval = 5 * time.Millisecond
	cfg.LeaseDuration = 3 * time.Second
	cfg.ReapInterval = time.Hour
	workerRuntime := jobs.NewWorker(store, cfg)
	workerRuntime.RegisterHandler(jobType, handler)
	workerRuntime.Start(context.Background())

	var stopOnce sync.Once
	var stopErr error
	stop := func() error {
		stopOnce.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stopErr = workerRuntime.StopGracefully(ctx)
		})
		return stopErr
	}
	t.Cleanup(func() { _ = stop() })
	return workerRuntime, stop
}

func awaitValidationFC06Handler(t *testing.T, handlerReturned <-chan struct{}) {
	t.Helper()
	select {
	case <-handlerReturned:
	case <-time.After(5 * time.Second):
		t.Fatal("FC-06 handler did not reach its post-finalization observation")
	}
}
