package jobs

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// VALIDATION-ONLY: convert these observations to desired-invariant regression
// tests during production hardening.
func TestValidationFC13BusinessRowsAffectedZeroStillFailsJob(t *testing.T) {
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
	if err := store.ConditionalFinalizeFailure(ctx, job.ID, workerID, claimToken, ErrorClassPermanent,
		"VALIDATION_FC13", "injected terminal failure", terminalReasonPointer(TerminalReasonPermanent), true, time.Time{}); err != nil {
		t.Fatalf("ConditionalFinalizeFailure: %v", err)
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
	if savedJob.Status != StatusFailed || snapshotStatus != "READY" || revisionStatus != "FAILED" {
		t.Fatalf("whole transaction observation: job=%s snapshot=%s revision=%s; want FAILED/READY/FAILED", savedJob.Status, snapshotStatus, revisionStatus)
	}
	t.Logf("observation: transaction committed Job FAILED while business snapshot UPDATE affected zero rows and READY state remained; revision=%s", revisionStatus)
}

// VALIDATION-ONLY: convert this observation to a desired-invariant regression
// test during production hardening.
func TestValidationFC13RevisionRowsAffectedZeroStillFailsJob(t *testing.T) {
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
	if err := store.ConditionalFinalizeFailure(ctx, job.ID, workerID, claimToken, ErrorClassPermanent,
		"VALIDATION_FC13", "injected terminal failure", terminalReasonPointer(TerminalReasonPermanent), true, time.Time{}); err != nil {
		t.Fatalf("ConditionalFinalizeFailure: %v", err)
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
	if savedJob.Status != StatusFailed || snapshotStatus != "FAILED" || revisionStatus != "READY" || revisionStage != "READY" {
		t.Fatalf("whole transaction observation: job=%s snapshot=%s revision=%s/%s; want FAILED/FAILED/READY/READY", savedJob.Status, snapshotStatus, revisionStatus, revisionStage)
	}
	t.Logf("observation: transaction committed Job FAILED and snapshot FAILED while incompatible revision UPDATE affected zero rows; revision remained %s/%s", revisionStatus, revisionStage)
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
