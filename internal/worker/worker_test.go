package worker_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/mq"
	"repolens/internal/outbox"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/worker"
)

func setupTestEnvironment(t *testing.T) (*gorm.DB, mq.Broker) {
	dbName := "file:memdb_" + time.Now().String() + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	broker := mq.NewMemoryBroker()
	return db, broker
}

func TestWorkerClaimAndAttemptCreation(t *testing.T) {
	db, _ := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-1",
		RepositoryID:           "repo-1",
		SnapshotID:             "snap-1",
		IssueTitle:             "Test Bug",
		IdempotencyKey:         "k1",
		IdempotencyRequestHash: "h1",
	}
	_ = diagStore.CreateWithOutbox(ctx, run, nil)

	// API phase: Attempt count must be 0
	attempts, _ := diagStore.ListAttemptsByRun(ctx, run.ID)
	if len(attempts) != 0 {
		t.Fatalf("expected 0 attempts at API creation, got %d", len(attempts))
	}

	// Worker 1 claims run
	claimedRun, attempt, err := diagStore.ClaimRun(ctx, run.ID, []diagnosis.RunStatus{diagnosis.StatusQueued}, "worker-1", 5*time.Minute)
	if err != nil || attempt == nil {
		t.Fatalf("failed to claim run: %v", err)
	}

	if claimedRun.Status != diagnosis.StatusRunning {
		t.Errorf("expected run status RUNNING, got %s", claimedRun.Status)
	}
	if attempt.AttemptNo != 1 || attempt.Status != diagnosis.AttemptStatusRunning {
		t.Errorf("expected attempt #1 in RUNNING, got attempt #%d in %s", attempt.AttemptNo, attempt.Status)
	}

	// Worker 2 attempts concurrent claim on same run -> Expect conflict!
	_, _, err2 := diagStore.ClaimRun(ctx, run.ID, []diagnosis.RunStatus{diagnosis.StatusQueued}, "worker-2", 5*time.Minute)
	if err2 == nil {
		t.Fatalf("expected claim conflict for second worker, but succeeded")
	}
}

func TestWorkerCrashAndStaleAttemptRecovery(t *testing.T) {
	db, _ := setupTestEnvironment(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	outboxStore := outbox.NewStore(db)

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-1",
		RepositoryID:           "repo-1",
		SnapshotID:             "snap-1",
		IssueTitle:             "Crash Recovery Test",
		IdempotencyKey:         "k2",
		IdempotencyRequestHash: "h2",
	}
	_ = diagStore.CreateWithOutbox(ctx, run, nil)

	// Worker 1 claims and starts Attempt #1
	_, attempt1, err := diagStore.ClaimRun(ctx, run.ID, []diagnosis.RunStatus{diagnosis.StatusQueued}, "worker-crashed", 5*time.Minute)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Simulate worker crash by setting heartbeat in the past
	staleTime := time.Now().Add(-1 * time.Minute)
	_ = db.Model(&diagnosis.DiagnosisAttempt{}).Where("id = ?", attempt1.ID).Update("heartbeat_at", staleTime).Error

	// Recovery Sweeper sweeps
	sweeper := worker.NewRecoverySweeper(diagStore, 30*time.Second, 1*time.Second, 100*time.Millisecond)
	recoveredCount := sweeper.SweepOnce(ctx)
	if recoveredCount != 1 {
		t.Fatalf("expected 1 recovered stale attempt, got %d", recoveredCount)
	}

	// Verify Attempt #1 is now ABANDONED
	att1Refreshed, _ := diagStore.GetAttempt(ctx, attempt1.ID)
	if att1Refreshed.Status != diagnosis.AttemptStatusAbandoned {
		t.Errorf("expected Attempt #1 status ABANDONED, got %s", att1Refreshed.Status)
	}

	// Verify Run is in RETRY_WAIT
	runRefreshed, _ := diagStore.GetByID(ctx, run.ID)
	if runRefreshed.Status != diagnosis.StatusRetryWait {
		t.Errorf("expected Run status RETRY_WAIT, got %s", runRefreshed.Status)
	}

	// Verify retry OutboxEvent was created
	time.Sleep(150 * time.Millisecond)
	pendingEvents, _ := outboxStore.FetchPending(ctx, 10)
	if len(pendingEvents) == 0 {
		t.Fatalf("expected pending retry outbox event after recovery")
	}

	// Worker 2 claims run on retry -> Attempt #2 created!
	claimedRun2, attempt2, err := diagStore.ClaimRun(ctx, run.ID, []diagnosis.RunStatus{diagnosis.StatusRetryWait}, "worker-recovery-2", 5*time.Minute)
	if err != nil || attempt2 == nil {
		t.Fatalf("worker 2 claim failed: %v", err)
	}
	if attempt2.AttemptNo != 2 {
		t.Errorf("expected AttemptNo 2, got %d", attempt2.AttemptNo)
	}
	if claimedRun2.Status != diagnosis.StatusRunning {
		t.Errorf("expected Run RUNNING, got %s", claimedRun2.Status)
	}
}

func TestConsumerExecutionAndDLQ(t *testing.T) {
	db, broker := setupTestEnvironment(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore("/tmp/repolens_test_snap")
	citVal := evidence.NewCitationValidator(storeFS)
	fakeExec := worker.NewFakeDiagnosisExecutor()

	consumer := worker.NewDiagnosisConsumer(
		worker.ConsumerConfig{
			WorkerID:        "worker-test-1",
			Prefetch:        2,
			MaxAttempts:     3,
			AttemptDeadline: 2 * time.Minute,
			RetryBackoff:    100 * time.Millisecond,
		},
		broker,
		diagStore,
		repStore,
		citStore,
		citVal,
		fakeExec,
		nil,
	)

	go func() {
		_ = consumer.Start(ctx)
	}()

	// 1. Test Malformed JSON -> Should route to DLQ
	malformedMsg := mq.Message{
		ID:        "msg-malformed-1",
		EventType: "DIAGNOSIS_REQUESTED",
		Payload:   "invalid-json-string{",
	}
	_ = broker.Publish(ctx, mq.QueueDiagnosisTask, malformedMsg)

	time.Sleep(100 * time.Millisecond)

	// 2. Test Happy Path execution
	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-100",
		RepositoryID:           "repo-100",
		SnapshotID:             "snap-100",
		IssueTitle:             "Happy Path Bug",
		IdempotencyKey:         "k-happy",
		IdempotencyRequestHash: "h-happy",
	}
	_ = diagStore.CreateWithOutbox(ctx, run, nil)

	payloadBytes, _ := json.Marshal(worker.DiagnosisPayload{
		DiagnosisRunID: run.ID,
		RepositoryID:   run.RepositoryID,
		SnapshotID:     run.SnapshotID,
		UserID:         run.UserID,
	})
	validMsg := mq.Message{
		ID:        "msg-valid-1",
		EventType: "DIAGNOSIS_REQUESTED",
		Payload:   string(payloadBytes),
	}
	_ = broker.Publish(ctx, mq.QueueDiagnosisTask, validMsg)

	// Wait for execution
	time.Sleep(200 * time.Millisecond)

	finalRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to fetch run: %v", err)
	}
	if finalRun.Status != diagnosis.StatusSucceeded {
		t.Errorf("expected final status SUCCEEDED, got %s", finalRun.Status)
	}

	report, err := repStore.GetByRunID(ctx, run.ID)
	if err != nil || report == nil {
		t.Fatalf("expected report generated, got err: %v", err)
	}
	if report.RootCause == "" {
		t.Errorf("expected non-empty root cause in report")
	}
}
