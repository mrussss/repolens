package jobs

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestFC13BusinessUnexpectedTerminalStateRollsBack(t *testing.T) {
	db, store := setupValidationFC13Store(t)
	defer db.Close()
	ctx := context.Background()
	const snapshotID = "validation-fc13-business-ready"
	if _, err := db.ExecContext(ctx, `INSERT INTO repository_snapshots(id, status) VALUES (?, 'READY')`, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO analysis_revisions(id, snapshot_id, status, stage, version) VALUES (?, ?, 'PREPARING', 'MATERIALIZING', 1)`, "validation-fc13-revision-business", snapshotID); err != nil {
		t.Fatal(err)
	}
	job, workerID, claimToken := claimValidationFC13StageJob(t, ctx, store, snapshotID)
	err := store.ConditionalFinalizeFailure(ctx, job.ID, workerID, claimToken, ErrorClassPermanent,
		"FC13", "injected terminal failure", terminalReasonPointer(TerminalReasonPermanent), true, time.Time{})
	if err == nil || !strings.Contains(err.Error(), ErrorCodeAtomicFinalizationStateConflict) {
		t.Fatalf("ConditionalFinalizeFailure error = %v, want %s conflict", err, ErrorCodeAtomicFinalizationStateConflict)
	}

	savedJob, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshotStatus, revisionStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM repository_snapshots WHERE id = ?`, snapshotID).Scan(&snapshotStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status FROM analysis_revisions WHERE snapshot_id = ?`, snapshotID).Scan(&revisionStatus); err != nil {
		t.Fatal(err)
	}
	if savedJob.Status != StatusRunning || snapshotStatus != "READY" || revisionStatus != "PREPARING" {
		t.Fatalf("whole transaction rollback: job=%s snapshot=%s revision=%s; want RUNNING/READY/PREPARING", savedJob.Status, snapshotStatus, revisionStatus)
	}
}

func TestFC13RevisionUnexpectedTerminalStateRollsBackBusinessFailure(t *testing.T) {
	db, store := setupValidationFC13Store(t)
	defer db.Close()
	ctx := context.Background()
	const snapshotID = "validation-fc13-revision-ready"
	if _, err := db.ExecContext(ctx, `INSERT INTO repository_snapshots(id, status) VALUES (?, 'MATERIALIZING')`, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO analysis_revisions(id, snapshot_id, status, stage, version) VALUES (?, ?, 'READY', 'READY', 3)`, "validation-fc13-revision-incompatible", snapshotID); err != nil {
		t.Fatal(err)
	}
	job, workerID, claimToken := claimValidationFC13StageJob(t, ctx, store, snapshotID)
	err := store.ConditionalFinalizeFailure(ctx, job.ID, workerID, claimToken, ErrorClassPermanent,
		"FC13", "injected terminal failure", terminalReasonPointer(TerminalReasonPermanent), true, time.Time{})
	if err == nil || !strings.Contains(err.Error(), ErrorCodeAtomicFinalizationStateConflict) {
		t.Fatalf("ConditionalFinalizeFailure error = %v, want %s conflict", err, ErrorCodeAtomicFinalizationStateConflict)
	}

	savedJob, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshotStatus, revisionStatus, revisionStage string
	if err := db.QueryRowContext(ctx, `SELECT status FROM repository_snapshots WHERE id = ?`, snapshotID).Scan(&snapshotStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT status, stage FROM analysis_revisions WHERE snapshot_id = ?`, snapshotID).Scan(&revisionStatus, &revisionStage); err != nil {
		t.Fatal(err)
	}
	if savedJob.Status != StatusRunning || snapshotStatus != "MATERIALIZING" || revisionStatus != "READY" || revisionStage != "READY" {
		t.Fatalf("whole transaction rollback: job=%s snapshot=%s revision=%s/%s; want RUNNING/MATERIALIZING/READY/READY", savedJob.Status, snapshotStatus, revisionStatus, revisionStage)
	}
}

func TestFC13AlreadyFailedBusinessAndRevisionAreIdempotent(t *testing.T) {
	db, store := setupValidationFC13Store(t)
	defer db.Close()
	ctx := context.Background()
	const snapshotID = "fc13-already-failed"
	if _, err := db.ExecContext(ctx, `INSERT INTO repository_snapshots(id, status) VALUES (?, 'FAILED')`, snapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO analysis_revisions(id, snapshot_id, status, stage, version) VALUES (?, ?, 'FAILED', 'MATERIALIZING', 2)`, "fc13-already-failed-revision", snapshotID); err != nil {
		t.Fatal(err)
	}
	job, workerID, claimToken := claimValidationFC13StageJob(t, ctx, store, snapshotID)
	if err := store.ConditionalFinalizeFailure(ctx, job.ID, workerID, claimToken, ErrorClassPermanent,
		"FC13", "injected terminal failure", terminalReasonPointer(TerminalReasonPermanent), true, time.Time{}); err != nil {
		t.Fatalf("already failed terminal objects should be idempotent: %v", err)
	}
	savedJob, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if savedJob.Status != StatusFailed {
		t.Fatalf("job status = %s, want FAILED", savedJob.Status)
	}
}

func TestFC13MissingRevisionRowRemainsNoop(t *testing.T) {
	db, store := setupValidationFC13Store(t)
	defer db.Close()
	ctx := context.Background()
	const snapshotID = "fc13-no-revision"
	if _, err := db.ExecContext(ctx, `INSERT INTO repository_snapshots(id, status) VALUES (?, 'MATERIALIZING')`, snapshotID); err != nil {
		t.Fatal(err)
	}
	job, workerID, claimToken := claimValidationFC13StageJob(t, ctx, store, snapshotID)
	if err := store.ConditionalFinalizeFailure(ctx, job.ID, workerID, claimToken, ErrorClassPermanent,
		"FC13", "injected terminal failure", terminalReasonPointer(TerminalReasonPermanent), true, time.Time{}); err != nil {
		t.Fatalf("missing legacy revision row should retain no-op behavior: %v", err)
	}
	savedJob, err := store.GetJobByID(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	var snapshotStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM repository_snapshots WHERE id = ?`, snapshotID).Scan(&snapshotStatus); err != nil {
		t.Fatal(err)
	}
	if savedJob.Status != StatusFailed || snapshotStatus != "FAILED" {
		t.Fatalf("legacy no-revision completion: job=%s snapshot=%s, want FAILED/FAILED", savedJob.Status, snapshotStatus)
	}
}

func setupValidationFC13Store(t *testing.T) (*sql.DB, *Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "fc13.db")
	db, err := sql.Open("sqlite3", dbPath+"?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	statements := []string{
		`CREATE TABLE analysis_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_type TEXT NOT NULL, resource_id TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'PENDING',
			execution_generation INTEGER NOT NULL DEFAULT 1, terminal_reason TEXT,
			attempt_count INTEGER NOT NULL DEFAULT 0, max_attempts INTEGER NOT NULL DEFAULT 3,
			next_run_at DATETIME NOT NULL, worker_id TEXT, claim_token TEXT, lease_until DATETIME,
			cancel_requested BOOLEAN NOT NULL DEFAULT 0, last_error_class TEXT, last_error_code TEXT,
			last_error_message TEXT, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
			finished_at DATETIME, execution_started BOOLEAN NOT NULL DEFAULT 0,
			UNIQUE(job_type, resource_id)
		)`,
		`CREATE TABLE repository_snapshots (id TEXT PRIMARY KEY, status TEXT NOT NULL, error_code TEXT)`,
		`CREATE TABLE analysis_revisions (
			id TEXT PRIMARY KEY, snapshot_id TEXT, status TEXT NOT NULL, stage TEXT NOT NULL,
			error_code TEXT, error_message TEXT, version INTEGER NOT NULL, updated_at DATETIME
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("create FC-13 SQLite schema: %v", err)
		}
	}
	return db, NewStoreWithDriver(db, "sqlite3")
}

func claimValidationFC13StageJob(t *testing.T, ctx context.Context, store *Store, snapshotID string) (*AnalysisJob, string, string) {
	t.Helper()
	const workerID = "validation-fc13-worker"
	job := &AnalysisJob{
		JobType: JobTypeMaterializeSnapshot, ResourceID: snapshotID, Status: StatusPending,
		ExecutionGeneration: 1, MaxAttempts: 3, NextRunAt: time.Now().UTC(),
	}
	if err := store.CreateJob(ctx, job); err != nil {
		t.Fatalf("create pending stage job: %v", err)
	}
	claimed, err := store.ClaimJobs(ctx, workerID, 1, time.Minute)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim stage job: claimed=%d err=%v", len(claimed), err)
	}
	started, err := store.MarkExecutionStarted(ctx, claimed[0].ID, workerID, *claimed[0].ClaimToken, claimed[0].ExecutionGeneration)
	if err != nil || started != 1 {
		t.Fatalf("mark stage job execution started: attempts=%d err=%v", started, err)
	}
	return claimed[0], workerID, *claimed[0].ClaimToken
}

func terminalReasonPointer(reason TerminalReason) *TerminalReason { return &reason }
