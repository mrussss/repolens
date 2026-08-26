package diagnosis_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/diagnosis"
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
		{diagnosis.StatusRunning, diagnosis.StatusRetryWait, true},
		{diagnosis.StatusRunning, diagnosis.StatusFailed, true},
		{diagnosis.StatusRetryWait, diagnosis.StatusRunning, true},
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
