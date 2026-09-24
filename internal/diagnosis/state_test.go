package diagnosis_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
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
	codeIndexBuildID := int64(101)
	retrievalBuildID := int64(202)

	// 1. First creation -> SUCCESS (QUEUED)
	run1, created1, err := diagSvc.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID:           userID,
		RepositoryID:     repoID,
		SnapshotID:       snapID,
		IssueTitle:       "Crash in worker",
		IssueDescription: "Goroutine panicked",
		ErrorLog:         "nil pointer",
		IdempotencyKey:   idempKey,
		CodeIndexBuildID: codeIndexBuildID,
		RetrievalBuildID: retrievalBuildID,
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
		CodeIndexBuildID: codeIndexBuildID,
		RetrievalBuildID: retrievalBuildID,
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
		CodeIndexBuildID: codeIndexBuildID,
		RetrievalBuildID: retrievalBuildID,
	})
	if err != diagnosis.ErrIdempotencyConflict {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
}

func TestCreateRequiresPinnedBuildIDs(t *testing.T) {
	db := setupTestDB(t)
	svc := diagnosis.NewService(diagnosis.NewStore(db), repo.NewStore(db), snapshot.NewStore(db))

	_, _, err := svc.Create(context.Background(), diagnosis.CreateDiagnosisInput{
		UserID:       "user-builds",
		RepositoryID: "repo-builds",
		SnapshotID:   "snapshot-builds",
		IssueTitle:   "missing build IDs",
	})
	if !errors.Is(err, diagnosis.ErrInvalidBuildSelection) {
		t.Fatalf("error = %v, want ErrInvalidBuildSelection", err)
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

func TestFinalizeSuccessRollsBackEveryWriteStage(t *testing.T) {
	tests := []struct {
		name    string
		trigger string
	}{
		{name: "report insert", trigger: `CREATE TRIGGER inject_report_failure BEFORE INSERT ON reports BEGIN SELECT RAISE(ABORT, 'injected report failure'); END`},
		{name: "citation insert", trigger: `CREATE TRIGGER inject_citation_failure BEFORE INSERT ON citations BEGIN SELECT RAISE(ABORT, 'injected citation failure'); END`},
		{name: "attempt update", trigger: `CREATE TRIGGER inject_attempt_failure BEFORE UPDATE OF status ON diagnosis_attempts WHEN NEW.status = 'SUCCEEDED' BEGIN SELECT RAISE(ABORT, 'injected attempt failure'); END`},
		{name: "run update", trigger: `CREATE TRIGGER inject_run_failure BEFORE UPDATE OF status ON diagnosis_runs WHEN NEW.status = 'SUCCEEDED' BEGIN SELECT RAISE(ABORT, 'injected run failure'); END`},
		{name: "job update", trigger: `CREATE TRIGGER inject_job_failure BEFORE UPDATE OF status ON analysis_jobs WHEN NEW.status = 'SUCCEEDED' BEGIN SELECT RAISE(ABORT, 'injected job failure'); END`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, store, run, job, attempt, workerID, claimToken := prepareInvalidFinalization(t)
			defer func() { raw, _ := db.DB(); _ = raw.Close() }()
			if err := db.Exec(tc.trigger).Error; err != nil {
				t.Fatal(err)
			}
			report := &evidence.Report{ID: "success-stage-report", DiagnosisRunID: run.ID, AttemptID: attempt.ID, RootCause: "root", FindingsJSON: "[]", RecommendedChecksJSON: "[]", StructuredPayloadJSON: "{}", LimitationsJSON: "[]"}
			citation := evidence.Citation{ID: "success-stage-citation", ReportID: report.ID, SnapshotID: run.SnapshotID, FilePath: "main.go", StartLine: 1, EndLine: 1}
			if err := store.FinalizeSuccess(context.Background(), job.ID, workerID, claimToken, run.ID, attempt.ID, report, []evidence.Citation{citation}, 1, 1, 1); err == nil {
				t.Fatal("injected finalizer stage failure was ignored")
			}
			var savedRun diagnosis.DiagnosisRun
			var savedAttempt diagnosis.DiagnosisAttempt
			var savedJob jobs.AnalysisJob
			var reports, citations int64
			if err := db.First(&savedRun, "id = ?", run.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&savedAttempt, "id = ?", attempt.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&savedJob, job.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&evidence.Report{}).Where("diagnosis_run_id = ?", run.ID).Count(&reports).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&evidence.Citation{}).Where("report_id = ?", report.ID).Count(&citations).Error; err != nil {
				t.Fatal(err)
			}
			if reports != 0 || citations != 0 || savedRun.Status != diagnosis.StatusRunning || savedAttempt.Status != diagnosis.AttemptStatusRunning || savedJob.Status != jobs.StatusRunning {
				t.Fatalf("%s failure was not fully rolled back: reports=%d citations=%d run=%s attempt=%s job=%s", tc.name, reports, citations, savedRun.Status, savedAttempt.Status, savedJob.Status)
			}
		})
	}
}

func prepareInvalidFinalization(t *testing.T) (*gorm.DB, *diagnosis.GormStore, *diagnosis.DiagnosisRun, *jobs.AnalysisJob, *diagnosis.DiagnosisAttempt, string, string) {
	t.Helper()
	db := setupTestDB(t)
	ctx := context.Background()
	store := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-invalid-finalize", UserID: "user-invalid-finalize", RepositoryID: "repo-invalid-finalize", SnapshotID: "snap-invalid-finalize",
		IssueTitle: "invalid", IdempotencyKey: "invalid-finalize-key", IdempotencyRequestHash: "invalid-finalize-hash",
	}
	if err := store.Create(ctx, run); err != nil {
		t.Fatal(err)
	}
	var job jobs.AnalysisJob
	if err := db.Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).First(&job).Error; err != nil {
		t.Fatal(err)
	}
	workerID, claimToken := "worker-invalid-finalize", "claim-invalid-finalize"
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Updates(map[string]interface{}{
		"status": jobs.StatusRunning, "worker_id": workerID, "claim_token": claimToken,
	}).Error; err != nil {
		t.Fatal(err)
	}
	attempt := &diagnosis.DiagnosisAttempt{ID: "attempt-invalid-finalize", DiagnosisRunID: run.ID, AttemptNo: 1, WorkerID: workerID}
	if err := store.StartAttempt(ctx, run.ID, attempt); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&job, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	return db, store, run, &job, attempt, workerID, claimToken
}

func invalidReport(runID, attemptID string) *evidence.Report {
	return &evidence.Report{
		ID: runID + "-invalid-report", DiagnosisRunID: runID, AttemptID: attemptID,
		ReportStatus: evidence.ReportInvalid, FindingsJSON: "[]", RecommendedChecksJSON: "[]",
		StructuredPayloadJSON: "{}", LimitationsJSON: "[]", RawOutput: "not-json", ParseError: "malformed JSON",
	}
}

func TestFinalizeInvalidStructuredReportIsAtomicAndFenced(t *testing.T) {
	db, store, run, job, attempt, workerID, claimToken := prepareInvalidFinalization(t)
	ctx := context.Background()
	report := invalidReport(run.ID, attempt.ID)
	err := store.FinalizeInvalidStructuredReport(ctx, job.ID, workerID, claimToken, run.ID, attempt.ID, report, 3, 4, 1, "INVALID_STRUCTURED_REPORT", report.ParseError)
	if err != nil {
		t.Fatal(err)
	}
	var savedRun diagnosis.DiagnosisRun
	var savedAttempt diagnosis.DiagnosisAttempt
	var savedReport evidence.Report
	var savedJob jobs.AnalysisJob
	if err := db.First(&savedRun, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&savedAttempt, "id = ?", attempt.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&savedReport, "id = ?", report.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&savedJob, job.ID).Error; err != nil {
		t.Fatal(err)
	}
	if savedRun.Status != diagnosis.StatusFailed || savedAttempt.Status != diagnosis.AttemptStatusFailedTerminal || savedJob.Status != jobs.StatusFailed {
		t.Fatalf("terminal states = run=%s attempt=%s job=%s", savedRun.Status, savedAttempt.Status, savedJob.Status)
	}
	if savedReport.ReportStatus != evidence.ReportInvalid || savedReport.RawOutput != report.RawOutput || savedReport.ParseError != report.ParseError || savedAttempt.RawOutput != report.RawOutput {
		t.Fatalf("invalid report/debug data not preserved: report=%+v attempt=%+v", savedReport, savedAttempt)
	}

	if err := store.FinalizeInvalidStructuredReport(ctx, job.ID, workerID, "stale-token", run.ID, attempt.ID, invalidReport(run.ID, attempt.ID+"-stale"), 0, 0, 0, "INVALID_STRUCTURED_REPORT", "stale"); !errors.Is(err, jobs.ErrAlreadyFinalized) {
		t.Fatalf("terminal duplicate = %v, want ErrAlreadyFinalized", err)
	}
}

func TestFinalizeInvalidStructuredReportRollsBackOnReportFailure(t *testing.T) {
	db, store, run, job, attempt, workerID, claimToken := prepareInvalidFinalization(t)
	ctx := context.Background()
	if err := db.Exec(`CREATE TRIGGER fail_invalid_report BEFORE INSERT ON reports BEGIN SELECT RAISE(ABORT, 'injected report failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER fail_invalid_report")
	err := store.FinalizeInvalidStructuredReport(ctx, job.ID, workerID, claimToken, run.ID, attempt.ID, invalidReport(run.ID, attempt.ID), 0, 0, 0, "INVALID_STRUCTURED_REPORT", "parse error")
	if err == nil {
		t.Fatal("expected injected report failure")
	}
	assertInvalidFinalizationRolledBack(t, db, run.ID, attempt.ID, job.ID)
}

func TestFinalizeInvalidStructuredReportRollsBackOnAttemptFailure(t *testing.T) {
	db, store, run, job, attempt, workerID, claimToken := prepareInvalidFinalization(t)
	ctx := context.Background()
	if err := db.Exec(`CREATE TRIGGER fail_invalid_attempt BEFORE UPDATE OF status ON diagnosis_attempts WHEN NEW.status = 'FAILED_TERMINAL' BEGIN SELECT RAISE(ABORT, 'injected attempt failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER fail_invalid_attempt")
	err := store.FinalizeInvalidStructuredReport(ctx, job.ID, workerID, claimToken, run.ID, attempt.ID, invalidReport(run.ID, attempt.ID), 0, 0, 0, "INVALID_STRUCTURED_REPORT", "parse error")
	if err == nil {
		t.Fatal("expected injected attempt failure")
	}
	assertInvalidFinalizationRolledBack(t, db, run.ID, attempt.ID, job.ID)
}

func TestFinalizeInvalidStructuredReportRollsBackOnRunFailure(t *testing.T) {
	db, store, run, job, attempt, workerID, claimToken := prepareInvalidFinalization(t)
	ctx := context.Background()
	if err := db.Exec(`CREATE TRIGGER fail_invalid_run BEFORE UPDATE OF status ON diagnosis_runs WHEN NEW.status = 'FAILED' BEGIN SELECT RAISE(ABORT, 'injected run failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP TRIGGER fail_invalid_run")
	err := store.FinalizeInvalidStructuredReport(ctx, job.ID, workerID, claimToken, run.ID, attempt.ID, invalidReport(run.ID, attempt.ID), 0, 0, 0, "INVALID_STRUCTURED_REPORT", "parse error")
	if err == nil {
		t.Fatal("expected injected run failure")
	}
	assertInvalidFinalizationRolledBack(t, db, run.ID, attempt.ID, job.ID)
}

func TestFinalizeInvalidStructuredReportDoesNotOverwriteCancellationOrStaleClaim(t *testing.T) {
	db, store, run, job, attempt, workerID, claimToken := prepareInvalidFinalization(t)
	ctx := context.Background()
	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Update("cancel_requested", true).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeInvalidStructuredReport(ctx, job.ID, workerID, claimToken, run.ID, attempt.ID, invalidReport(run.ID, attempt.ID), 0, 0, 0, "INVALID_STRUCTURED_REPORT", "parse error"); !errors.Is(err, jobs.ErrCancellationRequested) {
		t.Fatalf("cancelled job = %v, want ErrCancellationRequested", err)
	}
	assertInvalidFinalizationRolledBack(t, db, run.ID, attempt.ID, job.ID)

	if err := db.Model(&jobs.AnalysisJob{}).Where("id = ?", job.ID).Updates(map[string]interface{}{"cancel_requested": false, "claim_token": "new-token"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeInvalidStructuredReport(ctx, job.ID, workerID, claimToken, run.ID, attempt.ID, invalidReport(run.ID, attempt.ID+"-stale"), 0, 0, 0, "INVALID_STRUCTURED_REPORT", "parse error"); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale claim = %v, want ErrOwnershipLost", err)
	}
	assertInvalidFinalizationRolledBack(t, db, run.ID, attempt.ID, job.ID)
}

func assertInvalidFinalizationRolledBack(t *testing.T, db *gorm.DB, runID, attemptID string, jobID int64) {
	t.Helper()
	var run diagnosis.DiagnosisRun
	var attempt diagnosis.DiagnosisAttempt
	var job jobs.AnalysisJob
	var reportCount int64
	if err := db.First(&run, "id = ?", runID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&attempt, "id = ?", attemptID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&job, jobID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&evidence.Report{}).Where("diagnosis_run_id = ?", runID).Count(&reportCount).Error; err != nil {
		t.Fatal(err)
	}
	if reportCount != 0 || run.Status != diagnosis.StatusRunning || attempt.Status != diagnosis.AttemptStatusRunning || job.Status != jobs.StatusRunning {
		t.Fatalf("transaction was not rolled back: reports=%d run=%s attempt=%s job=%s", reportCount, run.Status, attempt.Status, job.Status)
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
	if queuedRun.Status != diagnosis.StatusCancelled || queuedRun.FinalAttemptID != "" {
		t.Fatalf("queued run terminal state = %s final_attempt_id=%q; queued cancellation must not invent an attempt", queuedRun.Status, queuedRun.FinalAttemptID)
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
	if runningRun.Status != diagnosis.StatusCancelled || runningRun.FinalAttemptID != attempt.ID {
		t.Fatalf("running run terminal state = %s final_attempt_id=%q; want cancelled with attempt %s", runningRun.Status, runningRun.FinalAttemptID, attempt.ID)
	}
	if err := db.First(&flagJob, runningJob.ID).Error; err != nil {
		t.Fatal(err)
	}
	if flagJob.Status != jobs.StatusCancelled {
		t.Fatalf("running job status = %s", flagJob.Status)
	}
}

func newRunningAttempt(t *testing.T, db *gorm.DB, store *diagnosis.GormStore, runID, attemptID string, generation, attemptNo int) *diagnosis.DiagnosisRun {
	t.Helper()
	ctx := context.Background()
	run := &diagnosis.DiagnosisRun{
		ID: runID, UserID: "user-" + runID, RepositoryID: "repo", SnapshotID: "snap",
		IssueTitle: "attempt fencing", IdempotencyKey: "key-" + runID, IdempotencyRequestHash: "hash-" + runID,
	}
	var existing diagnosis.DiagnosisRun
	if err := db.First(&existing, "id = ?", runID).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		if err := store.Create(ctx, run); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	attempt := &diagnosis.DiagnosisAttempt{
		ID: attemptID, DiagnosisRunID: runID, ExecutionGeneration: generation, AttemptNo: attemptNo, WorkerID: "worker",
	}
	if err := store.StartAttempt(ctx, runID, attempt); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestCheckpointWritesAreTypedGenerationScopedAndFenced(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := diagnosis.NewStore(db)
	newRunningAttempt(t, db, store, "run-checkpoint-generations", "gen1-final", 1, 1)
	newRunningAttempt(t, db, store, "run-checkpoint-generations", "gen2-partial", 2, 1)
	newRunningAttempt(t, db, store, "run-checkpoint-generations", "gen2-final", 2, 2)

	final := diagnosis.AttemptCheckpoint{
		ExecutionGeneration: 1, Kind: diagnosis.CheckpointKindFinalValid,
		RawOutput: "final generation one", Structured: true,
	}
	if err := store.UpdateAttemptCheckpoint(ctx, "gen1-final", final); err != nil {
		t.Fatal(err)
	}
	partial := diagnosis.AttemptCheckpoint{
		ExecutionGeneration: 2, Kind: diagnosis.CheckpointKindPartialProviderFailure,
		RawOutput: "partial generation two", FinishReason: "tool_calls",
	}
	if err := store.UpdateAttemptCheckpoint(ctx, "gen2-partial", partial); err != nil {
		t.Fatal(err)
	}
	if checkpoint, err := store.GetLatestFinalCheckpoint(ctx, "run-checkpoint-generations", 2); err != nil || checkpoint != nil {
		t.Fatalf("partial checkpoint became replayable: checkpoint=%+v err=%v", checkpoint, err)
	}
	final.ExecutionGeneration = 2
	final.RawOutput = "final generation two"
	if err := store.UpdateAttemptCheckpointWithDraft(ctx, "gen2-final", final); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.GetLatestFinalCheckpoint(ctx, "run-checkpoint-generations", 2)
	if err != nil || checkpoint == nil || checkpoint.ID != "gen2-final" {
		t.Fatalf("generation 2 checkpoint = %+v err=%v, want gen2-final", checkpoint, err)
	}
	if older, err := store.GetLatestFinalCheckpoint(ctx, "run-checkpoint-generations", 1); err != nil || older == nil || older.ID != "gen1-final" {
		t.Fatalf("generation 1 checkpoint = %+v err=%v, want gen1-final", older, err)
	}
	var savedPartial, savedFinal diagnosis.DiagnosisAttempt
	if err := db.First(&savedPartial, "id = ?", "gen2-partial").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&savedFinal, "id = ?", "gen2-final").Error; err != nil {
		t.Fatal(err)
	}
	if savedPartial.ProviderCompletedAt != nil {
		t.Fatal("partial checkpoint was marked provider-complete")
	}
	if savedFinal.ProviderCompletedAt == nil {
		t.Fatal("final checkpoint is missing provider_completed_at")
	}

	if err := store.FinishAttempt(ctx, "run-checkpoint-generations", "gen2-final", diagnosis.AttemptStatusSucceeded, 0, 0, 0, "", "", false); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateAttemptCheckpointWithDraft(ctx, "gen2-final", final); !errors.Is(err, diagnosis.ErrAttemptNotRunning) {
		t.Fatalf("checkpoint update after terminal transition = %v, want ErrAttemptNotRunning", err)
	}
	if err := store.UpdateAttemptCheckpoint(ctx, "missing-attempt", final); !errors.Is(err, diagnosis.ErrAttemptNotRunning) {
		t.Fatalf("checkpoint update for missing attempt = %v, want ErrAttemptNotRunning", err)
	}
}

func TestLegacyUntypedCheckpointIsNeverAutomaticallyReplayable(t *testing.T) {
	db := setupTestDB(t)
	store := diagnosis.NewStore(db)
	newRunningAttempt(t, db, store, "run-legacy-checkpoint", "attempt-legacy-checkpoint", 1, 1)
	completedAt := time.Now().UTC()
	if err := db.Model(&diagnosis.DiagnosisAttempt{}).Where("id = ?", "attempt-legacy-checkpoint").Updates(map[string]interface{}{
		"checkpoint_kind":       diagnosis.CheckpointKindLegacyUntyped,
		"provider_completed_at": completedAt,
		"raw_output":            `{"conclusion_kind":"ROOT_CAUSE","summary":"old","root_cause":"old","findings":[]}`,
	}).Error; err != nil {
		t.Fatal(err)
	}
	checkpoint, err := store.GetLatestFinalCheckpoint(context.Background(), "run-legacy-checkpoint", 1)
	if err != nil || checkpoint != nil {
		t.Fatalf("legacy checkpoint was automatically replayed: checkpoint=%+v err=%v", checkpoint, err)
	}
}

func TestStartAttemptRejectsPreviouslyUsedID(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	store := diagnosis.NewStore(db)
	run := newRunningAttempt(t, db, store, "run-attempt-id-reuse", "attempt-id-reuse", 1, 1)
	if err := store.FinishAttempt(ctx, run.ID, "attempt-id-reuse", diagnosis.AttemptStatusFailedTerminal, 0, 0, 0, "FAILED", "failed", false); err != nil {
		t.Fatal(err)
	}
	reused := &diagnosis.DiagnosisAttempt{
		ID: "attempt-id-reuse", DiagnosisRunID: run.ID, ExecutionGeneration: 2, AttemptNo: 1, WorkerID: "retry-worker",
	}
	if err := store.StartAttempt(ctx, run.ID, reused); !errors.Is(err, diagnosis.ErrAttemptAlreadyExists) {
		t.Fatalf("StartAttempt with reused ID = %v, want ErrAttemptAlreadyExists", err)
	}
	var saved diagnosis.DiagnosisAttempt
	if err := db.First(&saved, "id = ?", "attempt-id-reuse").Error; err != nil {
		t.Fatal(err)
	}
	if saved.ExecutionGeneration != 1 || saved.AttemptNo != 1 || saved.Status != diagnosis.AttemptStatusFailedTerminal {
		t.Fatalf("old attempt was modified by duplicate start: %+v", saved)
	}
}

func TestFinishAttemptAndRunFencesAttemptAndRunTransitions(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*testing.T, *gorm.DB, *diagnosis.GormStore, *diagnosis.DiagnosisRun, *diagnosis.DiagnosisAttempt)
		attemptID string
		wantErr   error
	}{
		{
			name: "attempt already terminal",
			mutate: func(t *testing.T, _ *gorm.DB, store *diagnosis.GormStore, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) {
				t.Helper()
				if err := store.FinishAttempt(context.Background(), run.ID, attempt.ID, diagnosis.AttemptStatusFailedRetryable, 0, 0, 0, "", "", true); err != nil {
					t.Fatal(err)
				}
			},
			attemptID: "attempt-terminal",
			wantErr:   diagnosis.ErrAttemptNotRunning,
		},
		{
			name: "run already terminal",
			mutate: func(t *testing.T, db *gorm.DB, _ *diagnosis.GormStore, run *diagnosis.DiagnosisRun, _ *diagnosis.DiagnosisAttempt) {
				t.Helper()
				if err := db.Model(&diagnosis.DiagnosisRun{}).Where("id = ?", run.ID).Update("status", diagnosis.StatusCancelled).Error; err != nil {
					t.Fatal(err)
				}
			},
			attemptID: "attempt-run-terminal",
			wantErr:   diagnosis.ErrRunTransitionConflict,
		},
		{
			name:      "cancel requested run",
			attemptID: "attempt-cancel-fenced",
			mutate: func(t *testing.T, db *gorm.DB, _ *diagnosis.GormStore, run *diagnosis.DiagnosisRun, _ *diagnosis.DiagnosisAttempt) {
				t.Helper()
				if err := db.Model(&diagnosis.DiagnosisRun{}).Where("id = ?", run.ID).Update("cancel_requested", true).Error; err != nil {
					t.Fatal(err)
				}
			},
			wantErr: diagnosis.ErrRunTransitionConflict,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := setupTestDB(t)
			store := diagnosis.NewStore(db)
			runID := "run-" + strings.ReplaceAll(test.name, " ", "-")
			attempt := &diagnosis.DiagnosisAttempt{ID: test.attemptID}
			run := newRunningAttempt(t, db, store, runID, test.attemptID, 1, 1)
			if err := db.First(attempt, "id = ?", test.attemptID).Error; err != nil {
				t.Fatal(err)
			}
			test.mutate(t, db, store, run, attempt)
			err := store.FinishAttemptAndRun(context.Background(), run.ID, test.attemptID, diagnosis.StatusFailed, diagnosis.AttemptStatusFailedTerminal, 0, 0, 0, "FAILED", "failed", false, 0)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("FinishAttemptAndRun error = %v, want %v", err, test.wantErr)
			}
			var savedRun diagnosis.DiagnosisRun
			var savedAttempt diagnosis.DiagnosisAttempt
			if err := db.First(&savedRun, "id = ?", run.ID).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.First(&savedAttempt, "id = ?", test.attemptID).Error; err != nil {
				t.Fatal(err)
			}
			if test.name == "attempt already terminal" {
				if savedRun.Status != diagnosis.StatusRunning || savedAttempt.Status != diagnosis.AttemptStatusFailedRetryable || savedRun.FinalAttemptID != "" {
					t.Fatalf("fenced terminal attempt mutated run: run=%+v attempt=%+v", savedRun, savedAttempt)
				}
			} else if test.name == "run already terminal" {
				if savedRun.Status != diagnosis.StatusCancelled || savedAttempt.Status != diagnosis.AttemptStatusRunning {
					t.Fatalf("run transition conflict was not rolled back: run=%+v attempt=%+v", savedRun, savedAttempt)
				}
			} else if savedRun.Status != diagnosis.StatusRunning || savedAttempt.Status != diagnosis.AttemptStatusRunning || savedRun.FinalAttemptID != "" {
				t.Fatalf("cancel fence was not preserved: run=%+v attempt=%+v", savedRun, savedAttempt)
			}
		})
	}

	t.Run("wrong run", func(t *testing.T) {
		db := setupTestDB(t)
		store := diagnosis.NewStore(db)
		first := newRunningAttempt(t, db, store, "run-wrong-first", "attempt-wrong-first", 1, 1)
		newRunningAttempt(t, db, store, "run-wrong-second", "attempt-wrong-second", 1, 1)
		err := store.FinishAttemptAndRun(context.Background(), first.ID, "attempt-wrong-second", diagnosis.StatusFailed, diagnosis.AttemptStatusFailedTerminal, 0, 0, 0, "FAILED", "failed", false, 0)
		if !errors.Is(err, diagnosis.ErrAttemptNotRunning) {
			t.Fatalf("wrong-run attempt transition error = %v, want ErrAttemptNotRunning", err)
		}
		var savedRun diagnosis.DiagnosisRun
		var savedAttempt diagnosis.DiagnosisAttempt
		if err := db.First(&savedRun, "id = ?", first.ID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.First(&savedAttempt, "id = ?", "attempt-wrong-second").Error; err != nil {
			t.Fatal(err)
		}
		if savedRun.Status != diagnosis.StatusRunning || savedAttempt.Status != diagnosis.AttemptStatusRunning || savedRun.FinalAttemptID != "" {
			t.Fatalf("wrong-run attempt mutated state: run=%+v attempt=%+v", savedRun, savedAttempt)
		}
	})

	t.Run("missing attempt", func(t *testing.T) {
		db := setupTestDB(t)
		store := diagnosis.NewStore(db)
		run := newRunningAttempt(t, db, store, "run-missing-attempt", "attempt-present", 1, 1)
		err := store.FinishAttemptAndRun(context.Background(), run.ID, "attempt-missing", diagnosis.StatusFailed, diagnosis.AttemptStatusFailedTerminal, 0, 0, 0, "FAILED", "failed", false, 0)
		if !errors.Is(err, diagnosis.ErrAttemptNotRunning) {
			t.Fatalf("missing attempt transition error = %v, want ErrAttemptNotRunning", err)
		}
		var savedRun diagnosis.DiagnosisRun
		var savedAttempt diagnosis.DiagnosisAttempt
		if err := db.First(&savedRun, "id = ?", run.ID).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.First(&savedAttempt, "id = ?", "attempt-present").Error; err != nil {
			t.Fatal(err)
		}
		if savedRun.Status != diagnosis.StatusRunning || savedAttempt.Status != diagnosis.AttemptStatusRunning || savedRun.FinalAttemptID != "" {
			t.Fatalf("missing attempt mutated state: run=%+v attempt=%+v", savedRun, savedAttempt)
		}
	})
}

func TestFinishAttemptAndRunConcurrentFinalizersCommitOnlyOnce(t *testing.T) {
	db := setupTestDB(t)
	store := diagnosis.NewStore(db)
	run := newRunningAttempt(t, db, store, "run-concurrent-finalize", "attempt-concurrent-finalize", 1, 1)
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			errs <- store.FinishAttemptAndRun(context.Background(), run.ID, "attempt-concurrent-finalize", diagnosis.StatusFailed, diagnosis.AttemptStatusFailedTerminal, 0, 0, 0, "FAILED", "failed", false, 0)
		}()
	}
	close(start)
	first, second := <-errs, <-errs
	successes := 0
	if first == nil {
		successes++
	}
	if second == nil {
		successes++
	}
	if successes != 1 {
		t.Fatalf("concurrent finalizer success count=%d errors=(%v,%v)", successes, first, second)
	}
	var savedRun diagnosis.DiagnosisRun
	var savedAttempt diagnosis.DiagnosisAttempt
	if err := db.First(&savedRun, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&savedAttempt, "id = ?", "attempt-concurrent-finalize").Error; err != nil {
		t.Fatal(err)
	}
	if savedRun.Status != diagnosis.StatusFailed || savedRun.FinalAttemptID != savedAttempt.ID || savedAttempt.Status != diagnosis.AttemptStatusFailedTerminal {
		t.Fatalf("concurrent finalization state = run=%+v attempt=%+v", savedRun, savedAttempt)
	}
}

func TestCloseAttemptDoesNotRequireOrMutateRunningRun(t *testing.T) {
	db := setupTestDB(t)
	store := diagnosis.NewStore(db)
	run := newRunningAttempt(t, db, store, "run-close-attempt-only", "attempt-close-only", 3, 1)
	if err := db.Model(&diagnosis.DiagnosisRun{}).Where("id = ?", run.ID).Updates(map[string]interface{}{
		"status": diagnosis.StatusSucceeded, "final_attempt_id": "winning-attempt",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.CloseAttempt(context.Background(), run.ID, "attempt-close-only", 4, diagnosis.AttemptStatusAbandoned, "FINALIZATION_OWNERSHIP_LOST", "wrong generation", false); !errors.Is(err, diagnosis.ErrAttemptNotRunning) {
		t.Fatalf("close with stale generation = %v, want ErrAttemptNotRunning", err)
	}
	var savedAttempt diagnosis.DiagnosisAttempt
	if err := db.First(&savedAttempt, "id = ?", "attempt-close-only").Error; err != nil {
		t.Fatal(err)
	}
	if savedAttempt.Status != diagnosis.AttemptStatusRunning {
		t.Fatalf("generation mismatch changed Attempt status to %s", savedAttempt.Status)
	}
	if err := store.CloseAttempt(context.Background(), run.ID, "attempt-close-only", 3, diagnosis.AttemptStatusAbandoned, "FINALIZATION_OWNERSHIP_LOST", "superseded by the winning finalizer", false); err != nil {
		t.Fatalf("close stale Attempt after Run terminalization: %v", err)
	}
	var savedRun diagnosis.DiagnosisRun
	if err := db.First(&savedRun, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&savedAttempt, "id = ?", "attempt-close-only").Error; err != nil {
		t.Fatal(err)
	}
	if savedRun.Status != diagnosis.StatusSucceeded || savedRun.FinalAttemptID != "winning-attempt" {
		t.Fatalf("closing stale Attempt changed the terminal Run: %+v", savedRun)
	}
	if savedAttempt.Status != diagnosis.AttemptStatusAbandoned || savedAttempt.FinishedAt == nil {
		t.Fatalf("stale Attempt was not closed: %+v", savedAttempt)
	}
}

func TestRecoverStaleAttemptRespectsLiveJobLease(t *testing.T) {
	db := setupTestDB(t)
	store := diagnosis.NewStore(db)
	run := newRunningAttempt(t, db, store, "run-live-attempt-lease", "attempt-live-lease", 1, 1)
	leaseUntil := time.Now().UTC().Add(time.Minute)
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).Updates(map[string]interface{}{
		"status": jobs.StatusRunning, "execution_generation": 1, "attempt_count": 1,
		"worker_id": "worker", "lease_until": leaseUntil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	err := store.RecoverStaleAttempt(context.Background(), "attempt-live-lease", run.ID, time.Second)
	if !errors.Is(err, diagnosis.ErrAttemptLeaseActive) {
		t.Fatalf("recovery with active lease = %v, want ErrAttemptLeaseActive", err)
	}
	var attempt diagnosis.DiagnosisAttempt
	if err := db.First(&attempt, "id = ?", "attempt-live-lease").Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != diagnosis.AttemptStatusRunning {
		t.Fatalf("live leased Attempt was changed to %s", attempt.Status)
	}

	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).Update("lease_until", time.Now().UTC().Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverStaleAttempt(context.Background(), "attempt-live-lease", run.ID, time.Second); err != nil {
		t.Fatalf("recovery after lease expiry: %v", err)
	}
	if err := db.First(&attempt, "id = ?", "attempt-live-lease").Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != diagnosis.AttemptStatusAbandoned {
		t.Fatalf("expired-lease Attempt status = %s, want ABANDONED", attempt.Status)
	}
}

func TestRecoverStaleAttemptDoesNotWaitForNewAttemptLease(t *testing.T) {
	db := setupTestDB(t)
	store := diagnosis.NewStore(db)
	run := newRunningAttempt(t, db, store, "run-new-attempt-live-lease", "attempt-old-crashed", 1, 1)
	leaseUntil := time.Now().UTC().Add(time.Minute)
	if err := db.Model(&jobs.AnalysisJob{}).Where("job_type = ? AND resource_id = ?", jobs.JobTypeRunDiagnosis, run.ID).Updates(map[string]interface{}{
		"status": jobs.StatusRunning, "execution_generation": 1, "attempt_count": 2,
		"worker_id": "retry-worker", "lease_until": leaseUntil,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := store.RecoverStaleAttempt(context.Background(), "attempt-old-crashed", run.ID, time.Second); err != nil {
		t.Fatalf("recover previous same-generation Attempt while retry has a live lease: %v", err)
	}
	var attempt diagnosis.DiagnosisAttempt
	if err := db.First(&attempt, "id = ?", "attempt-old-crashed").Error; err != nil {
		t.Fatal(err)
	}
	if attempt.Status != diagnosis.AttemptStatusAbandoned {
		t.Fatalf("old Attempt status = %s, want ABANDONED while newer attempt lease is live", attempt.Status)
	}
}
