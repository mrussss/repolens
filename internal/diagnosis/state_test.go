package diagnosis_test

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
	"repolens/internal/repo"
	"repolens/internal/snapshot"
)

func setupTestDB(t *testing.T) *gorm.DB {
	dbPath := filepath.Join(t.TempDir(), "diagnosis_test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite db: %v", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed to auto migrate test db: %v", err)
	}
	return db
}

func TestStateTransitions(t *testing.T) {
	tests := []struct {
		from  diagnosis.RunStatus
		to    diagnosis.RunStatus
		valid bool
	}{
		{diagnosis.StatusQueued, diagnosis.StatusRunning, true},
		{diagnosis.StatusQueued, diagnosis.StatusCancelled, true},
		{diagnosis.StatusRunning, diagnosis.StatusSucceeded, true},
		{diagnosis.StatusRunning, diagnosis.StatusFailed, true},
		{diagnosis.StatusSucceeded, diagnosis.StatusRunning, false},
		{diagnosis.StatusFailed, diagnosis.StatusRunning, false},
		{diagnosis.StatusCancelled, diagnosis.StatusRunning, false},
	}

	for _, tt := range tests {
		got := diagnosis.IsValidRunTransition(tt.from, tt.to)
		if got != tt.valid {
			t.Errorf("transition %s -> %s: expected valid=%v, got %v", tt.from, tt.to, tt.valid, got)
		}
	}
}

func TestRequestHashAndIdempotencyConflict(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	diagStore := diagnosis.NewStore(db)
	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	diagSvc := diagnosis.NewService(diagStore, repoStore, snapStore)

	userID := "user-123"
	repoID := "repo-123"
	snapID := "snap-123"

	// Create test repo and snapshot
	_ = repoStore.Create(ctx, &repo.Repository{
		ID:     repoID,
		UserID: userID,
		Name:   "test-repo",
		GitURL: "https://github.com/test/repo",
	})
	now := time.Now()
	_ = snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID:               snapID,
		RepositoryID:     repoID,
		CommitSHA:        "abc1234",
		Ref:              "main",
		MaterializedPath: "/tmp/snapshots/test",
		Status:           snapshot.StatusReady,
		ReadyAt:          &now,
	})

	idempKey := "idemp-test-key-1"

	// 1. First creation -> SUCCESS (QUEUED)
	run1, created1, err := diagSvc.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID:           userID,
		RepositoryID:     repoID,
		SnapshotID:       snapID,
		IssueTitle:       "Crash in worker",
		IssueDescription: "Goroutine panicked",
		ErrorLog:         "nil pointer",
		IdempotencyKey:   idempKey,
	})
	if err != nil || !created1 || run1 == nil {
		t.Fatalf("first creation failed: %v", err)
	}
	if run1.Status != diagnosis.StatusQueued {
		t.Errorf("expected initial status QUEUED, got %s", run1.Status)
	}

	// 2. Exact duplicate with same Idempotency-Key & Payload -> Returns existing record (is_duplicate=true, no error)
	run2, created2, err := diagSvc.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID:           userID,
		RepositoryID:     repoID,
		SnapshotID:       snapID,
		IssueTitle:       "Crash in worker",
		IssueDescription: "Goroutine panicked",
		ErrorLog:         "nil pointer",
		IdempotencyKey:   idempKey,
	})
	if err != nil || created2 || run2 == nil {
		t.Fatalf("duplicate creation should return existing run without error: %v", err)
	}
	if run2.ID != run1.ID {
		t.Errorf("expected same run ID %s, got %s", run1.ID, run2.ID)
	}

	// 3. Reused Idempotency-Key with DIFFERENT Payload -> 409 Conflict error
	_, _, err = diagSvc.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID:           userID,
		RepositoryID:     repoID,
		SnapshotID:       snapID,
		IssueTitle:       "Completely different issue title",
		IssueDescription: "Different description",
		ErrorLog:         "different log",
		IdempotencyKey:   idempKey,
	})
	if err != diagnosis.ErrIdempotencyConflict {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
}

func TestTransactionalJobCreation(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	diagStore := diagnosis.NewStore(db)

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-1",
		RepositoryID:           "repo-1",
		SnapshotID:             "snap-1",
		IssueTitle:             "Test Title",
		IdempotencyKey:         "key-99",
		IdempotencyRequestHash: "hash-99",
	}

	err := diagStore.Create(ctx, run)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	// Verify both run and analysis_job exist in DB
	savedRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil || savedRun == nil {
		t.Fatalf("saved run not found: %v", err)
	}

	sqlDB, _ := db.DB()
	jobsStore := jobs.NewStoreWithDriver(sqlDB, "sqlite3")
	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil || job == nil {
		t.Fatalf("expected analysis_job created atomically, got %v", err)
	}
	if job.Status != jobs.StatusPending {
		t.Errorf("expected job status PENDING, got %s", job.Status)
	}
}

func TestFinalizeSuccessIsFencedAndAtomic(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-finalize", UserID: "user-finalize", RepositoryID: "repo-finalize", SnapshotID: "snap-finalize",
		IssueTitle: "test", IdempotencyKey: "idempotency-finalize", IdempotencyRequestHash: "hash-finalize",
	}
	if err := diagStore.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	job := &jobs.AnalysisJob{}
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).First(job).Error; err != nil {
		t.Fatal(err)
	}
	workerID, claimToken := "worker-finalize", "claim-finalize"
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
		"status": jobs.StatusRunning, "worker_id": workerID, "claim_token": claimToken,
	}).Error; err != nil {
		t.Fatal(err)
	}
	attempt := &diagnosis.DiagnosisAttempt{ID: "attempt-finalize", DiagnosisRunID: run.ID, AttemptNo: 1, WorkerID: workerID}
	if err := diagStore.StartAttempt(ctx, run.ID, attempt); err != nil {
		t.Fatal(err)
	}

	// A duplicate citation ID forces a failure after report insertion. The
	// transaction must roll back the report and leave all states RUNNING.
	duplicate := &evidence.Citation{ID: "duplicate-citation", SnapshotID: run.SnapshotID, FilePath: "main.go", StartLine: 1, EndLine: 1}
	if err := db.Create(duplicate).Error; err != nil {
		t.Fatal(err)
	}
	report := &evidence.Report{ID: "report-rollback", DiagnosisRunID: run.ID, AttemptID: attempt.ID, RootCause: "root", FindingsJSON: "[]"}
	err := diagStore.FinalizeSuccess(ctx, job.ID, workerID, claimToken, run.ID, attempt.ID, report, []evidence.Citation{{ID: duplicate.ID, SnapshotID: run.SnapshotID, FilePath: "main.go", StartLine: 1, EndLine: 1}}, 1, 1, 1)
	if err == nil {
		t.Fatal("expected duplicate citation to fail atomic finalization")
	}
	var reportCount int64
	if err := db.Model(&evidence.Report{}).Where("id = ?", report.ID).Count(&reportCount).Error; err != nil {
		t.Fatal(err)
	}
	if reportCount != 0 {
		t.Fatalf("report was committed despite citation failure")
	}
	var savedRun diagnosis.DiagnosisRun
	if err := db.First(&savedRun, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if savedRun.Status != diagnosis.StatusRunning {
		t.Fatalf("run changed after failed finalization: %s", savedRun.Status)
	}

	// A stale claim is rejected before any new report/citation is written.
	err = diagStore.FinalizeSuccess(ctx, job.ID, workerID, "stale-claim", run.ID, attempt.ID, report, nil, 0, 0, 0)
	if !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("expected ownership fencing, got %v", err)
	}
}

func TestCancellationQueuedRunningAndFinalizeRace(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := diagnosis.NewStore(db)

	queued := &diagnosis.DiagnosisRun{ID: "run-cancel-queued", UserID: "user-cancel", RepositoryID: "repo", SnapshotID: "snap", IssueTitle: "queued", IdempotencyKey: "cancel-queued", IdempotencyRequestHash: "hash"}
	if err := store.Create(ctx, queued); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestCancellation(ctx, queued.ID, queued.UserID); err != nil {
		t.Fatal(err)
	}
	var queuedJob jobs.AnalysisJob
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, queued.ID).First(&queuedJob).Error; err != nil {
		t.Fatal(err)
	}
	if queuedJob.Status != jobs.StatusCancelled {
		t.Fatalf("queued job status = %s", queuedJob.Status)
	}
	var queuedRun diagnosis.DiagnosisRun
	if err := db.First(&queuedRun, "id = ?", queued.ID).Error; err != nil {
		t.Fatal(err)
	}
	if queuedRun.Status != diagnosis.StatusCancelled {
		t.Fatalf("queued run status = %s", queuedRun.Status)
	}

	running := &diagnosis.DiagnosisRun{ID: "run-cancel-running", UserID: "user-cancel", RepositoryID: "repo", SnapshotID: "snap", IssueTitle: "running", IdempotencyKey: "cancel-running", IdempotencyRequestHash: "hash"}
	if err := store.Create(ctx, running); err != nil {
		t.Fatal(err)
	}
	var runningJob jobs.AnalysisJob
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, running.ID).First(&runningJob).Error; err != nil {
		t.Fatal(err)
	}
	workerID, token := "worker-cancel", "claim-cancel"
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", runningJob.ID).Updates(map[string]interface{}{"status": jobs.StatusRunning, "worker_id": workerID, "claim_token": token}).Error; err != nil {
		t.Fatal(err)
	}
	attempt := &diagnosis.DiagnosisAttempt{ID: "attempt-cancel-running", DiagnosisRunID: running.ID, AttemptNo: 1, WorkerID: workerID}
	if err := store.StartAttempt(ctx, running.ID, attempt); err != nil {
		t.Fatal(err)
	}
	if err := store.RequestCancellation(ctx, running.ID, running.UserID); err != nil {
		t.Fatal(err)
	}
	var flagJob jobs.AnalysisJob
	if err := db.First(&flagJob, runningJob.ID).Error; err != nil {
		t.Fatal(err)
	}
	if !flagJob.CancelRequested {
		t.Fatal("running job cancellation flag was not set")
	}
	if err := store.FinalizeCancellation(ctx, runningJob.ID, workerID, token, running.ID, attempt.ID); err != nil {
		t.Fatal(err)
	}
	var cancelledAttempt diagnosis.DiagnosisAttempt
	if err := db.First(&cancelledAttempt, "id = ?", attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if cancelledAttempt.Status != diagnosis.AttemptStatusCancelled {
		t.Fatalf("attempt status = %s", cancelledAttempt.Status)
	}
	var runningRun diagnosis.DiagnosisRun
	if err := db.First(&runningRun, "id = ?", running.ID).Error; err != nil {
		t.Fatal(err)
	}
	if runningRun.Status != diagnosis.StatusCancelled {
		t.Fatalf("running run status = %s", runningRun.Status)
	}
	if err := db.First(&flagJob, runningJob.ID).Error; err != nil {
		t.Fatal(err)
	}
	if flagJob.Status != jobs.StatusCancelled {
		t.Fatalf("running job status = %s", flagJob.Status)
	}
}
