package integration_real

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"repolens/internal/diagnosis"
	"repolens/internal/outbox"
	"repolens/internal/platform/mysql"
	"repolens/internal/repo"
	"repolens/internal/snapshot"
)

func setupRealMySQL(t *testing.T) (*gorm.DB, func()) {
	ctx := context.Background()

	mysqlContainer, err := tcmysql.RunContainer(ctx,
		tc.WithImage("mysql:8.0"),
		tcmysql.WithDatabase("repolens_test"),
		tcmysql.WithUsername("testuser"),
		tcmysql.WithPassword("testpass"),
	)
	if err != nil {
		if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "1" {
			t.Fatalf("FAILED: real MySQL testcontainers required by release gate but failed to start: %v", err)
		}
		t.Skipf("Skipping real MySQL testcontainers test (Docker not available: %v)", err)
		return nil, nil
	}

	connStr, err := mysqlContainer.ConnectionString(ctx, "charset=utf8mb4&parseTime=True&loc=Local")
	if err != nil {
		_ = mysqlContainer.Terminate(ctx)
		t.Fatalf("failed to get connection string: %v", err)
	}

	db, err := gorm.Open(gormmysql.Open(connStr), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		_ = mysqlContainer.Terminate(ctx)
		t.Fatalf("failed to open real MySQL connection: %v", err)
	}

	if err := mysql.AutoMigrate(db); err != nil {
		_ = mysqlContainer.Terminate(ctx)
		t.Fatalf("failed to migrate real MySQL database: %v", err)
	}

	cleanup := func() {
		_ = mysqlContainer.Terminate(context.Background())
	}

	return db, cleanup
}

func TestRealMySQL_DiagnosisIdempotencyAndOutbox(t *testing.T) {
	db, cleanup := setupRealMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	diagSvc := diagnosis.NewService(diagStore, repoStore, snapStore)

	testRepo := &repo.Repository{
		ID:         "repo-real-mysql",
		UserID:     "user-1",
		Name:       "payment-svc",
		GitURL:     "https://github.com/example/payment-svc",
		DefaultRef: "main",
		Status:     "ACTIVE",
	}
	_ = repoStore.Create(ctx, testRepo)

	testSnap := &snapshot.RepositorySnapshot{
		ID:           "snap-real-mysql",
		RepositoryID: testRepo.ID,
		CommitSHA:    "abc123456789",
		Ref:          "main",
		Status:       snapshot.StatusReady,
	}
	_ = snapStore.Create(ctx, testSnap)

	input := diagnosis.CreateDiagnosisInput{
		UserID:           testRepo.UserID,
		RepositoryID:     testRepo.ID,
		SnapshotID:       testSnap.ID,
		IssueTitle:       "Deadlock in payment handler",
		IssueDescription: "Two concurrent transactions acquire row locks in reverse order",
		ErrorLog:         "Error 1213: Deadlock found when trying to get lock",
		IdempotencyKey:   "idemp-real-001",
	}

	// 1. Create DiagnosisRun and OutboxEvent transactionally on real MySQL
	run1, created1, err := diagSvc.Create(ctx, input)
	if err != nil || !created1 {
		t.Fatalf("first creation failed on real MySQL: %v", err)
	}
	if run1.Status != diagnosis.StatusQueued {
		t.Errorf("expected QUEUED status, got %s", run1.Status)
	}

	// Verify 0 attempts before worker claim
	attempts, err := diagStore.ListAttemptsByRun(ctx, run1.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("expected 0 attempts at API stage, got %d", len(attempts))
	}

	// 2. Duplicate submission with SAME payload -> Return existing Run
	run2, created2, err := diagSvc.Create(ctx, input)
	if err != nil || created2 {
		t.Fatalf("expected duplicate recognized, but created2=%v (err=%v)", created2, err)
	}
	if run2.ID != run1.ID {
		t.Errorf("expected returned run ID %s to match %s", run2.ID, run1.ID)
	}

	// 3. Duplicate submission with DIFFERENT payload -> 409 Conflict
	conflictInput := input
	conflictInput.IssueTitle = "Mismatched title for conflict check"
	_, _, errConflict := diagSvc.Create(ctx, conflictInput)
	if !errors.Is(errConflict, diagnosis.ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict on real MySQL, got: %v", errConflict)
	}
}

func TestRealMySQL_ConcurrentWorkerClaimFencing(t *testing.T) {
	db, cleanup := setupRealMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-conc-mysql",
		RepositoryID:           "repo-conc-mysql",
		SnapshotID:             "snap-conc-mysql",
		IssueTitle:             "Concurrent Claim MySQL Test",
		IdempotencyKey:         "k-conc-mysql",
		IdempotencyRequestHash: "h-conc-mysql",
	}
	outboxEvt := &outbox.OutboxEvent{}
	_ = diagStore.CreateWithOutbox(ctx, run, outboxEvt)

	// Simulate 10 workers concurrently claiming the run on real MySQL
	numWorkers := 10
	var successfulClaims int64
	var claimConflicts int64

	var wg sync.WaitGroup
	expectedStatuses := []diagnosis.RunStatus{diagnosis.StatusQueued}

	for i := 1; i <= numWorkers; i++ {
		wg.Add(1)
		workerID := fmt.Sprintf("worker-node-%d", i)
		go func(wID string) {
			defer wg.Done()
			_, attempt, err := diagStore.ClaimRun(ctx, run.ID, expectedStatuses, wID, 5*time.Minute)
			if err == nil && attempt != nil {
				atomic.AddInt64(&successfulClaims, 1)
			} else if errors.Is(err, diagnosis.ErrClaimConflict) {
				atomic.AddInt64(&claimConflicts, 1)
			}
		}(workerID)
	}
	wg.Wait()

	if successfulClaims != 1 {
		t.Fatalf("expected EXACTLY 1 worker to claim run on real MySQL, got %d", successfulClaims)
	}
	if claimConflicts != int64(numWorkers-1) {
		t.Fatalf("expected %d claim conflicts, got %d", numWorkers-1, claimConflicts)
	}

	// Verify only 1 Attempt exists on MySQL
	attempts, err := diagStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("expected exactly 1 attempt on real MySQL, got %d", len(attempts))
	}
	if attempts[0].AttemptNo != 1 || attempts[0].Status != diagnosis.AttemptStatusRunning {
		t.Errorf("expected Attempt #1 RUNNING, got %+v", attempts[0])
	}
}

func TestRealMySQL_StaleAttemptRecovery(t *testing.T) {
	db, cleanup := setupRealMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()

	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	outboxStore := outbox.NewStore(db)

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-stale-mysql",
		RepositoryID:           "repo-stale-mysql",
		SnapshotID:             "snap-stale-mysql",
		IssueTitle:             "Stale Recovery MySQL Test",
		IdempotencyKey:         "k-stale-mysql",
		IdempotencyRequestHash: "h-stale-mysql",
	}
	_ = diagStore.CreateWithOutbox(ctx, run, nil)

	// Worker 1 claims
	_, att1, err := diagStore.ClaimRun(ctx, run.ID, []diagnosis.RunStatus{diagnosis.StatusQueued}, "worker-crashed", 5*time.Minute)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Force heartbeat in the past
	staleHeartbeat := time.Now().Add(-2 * time.Minute)
	_ = db.Model(&diagnosis.DiagnosisAttempt{}).Where("id = ?", att1.ID).Update("heartbeat_at", staleHeartbeat).Error

	// Fetch stale attempts
	staleAttempts, err := diagStore.FetchStaleAttempts(ctx, 30*time.Second, 10)
	if err != nil || len(staleAttempts) != 1 {
		t.Fatalf("expected 1 stale attempt on real MySQL, got %d (err: %v)", len(staleAttempts), err)
	}

	// Recover stale attempt (backoff -1s so available_at <= now)
	err = diagStore.RecoverStaleAttempt(ctx, att1.ID, run.ID, -1*time.Second)
	if err != nil {
		t.Fatalf("failed to recover stale attempt on MySQL: %v", err)
	}

	// Verify Attempt #1 -> ABANDONED
	att1Refreshed, _ := diagStore.GetAttempt(ctx, att1.ID)
	if att1Refreshed.Status != diagnosis.AttemptStatusAbandoned {
		t.Errorf("expected Attempt #1 ABANDONED, got %s", att1Refreshed.Status)
	}

	// Verify Run -> RETRY_WAIT
	runRefreshed, _ := diagStore.GetByID(ctx, run.ID)
	if runRefreshed.Status != diagnosis.StatusRetryWait {
		t.Errorf("expected Run status RETRY_WAIT, got %s", runRefreshed.Status)
	}

	// Verify retry OutboxEvent created in MySQL
	pending, _ := outboxStore.FetchPending(ctx, 10)
	if len(pending) == 0 || pending[0].EventType != outbox.EventDiagnosisRetryRequested {
		t.Fatalf("expected retry outbox event in MySQL, got %+v", pending)
	}
}
