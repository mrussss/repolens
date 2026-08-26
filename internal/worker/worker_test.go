package worker_test

import (
	"context"
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
