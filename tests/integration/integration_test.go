package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/indexing"
	"repolens/internal/mq"
	"repolens/internal/outbox"
	"repolens/internal/platform/elasticsearch"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/retrieval"
	"repolens/internal/snapshot"
	"repolens/internal/worker"
)

func setupTestDB(t *testing.T) *gorm.DB {
	dbName := fmt.Sprintf("file:mem_%s?mode=memory&cache=shared", uuid.New().String())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed to migrate test database: %v", err)
	}
	return db
}

// 1. Test Diagnosis Creation & Idempotency
func TestDiagnosisCreationAndIdempotency(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)

	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	diagSvc := diagnosis.NewService(diagStore, repoStore, snapStore)

	// Seed Repo and Snapshot in READY status
	testRepo := &repo.Repository{
		ID:         "repo-100",
		UserID:     "user-100",
		Name:       "test-repo",
		GitURL:     "https://github.com/example/test-repo",
		DefaultRef: "main",
		Status:     "ACTIVE",
	}
	_ = repoStore.Create(ctx, testRepo)

	testSnap := &snapshot.RepositorySnapshot{
		ID:           "snap-100",
		RepositoryID: testRepo.ID,
		CommitSHA:    "abcdef123456",
		Ref:          "main",
		Status:       snapshot.StatusReady,
	}
	_ = snapStore.Create(ctx, testSnap)

	input := diagnosis.CreateDiagnosisInput{
		UserID:           "user-100",
		RepositoryID:     testRepo.ID,
		SnapshotID:       testSnap.ID,
		IssueTitle:       "Goroutine Leak Bug",
		IssueDescription: "Unbuffered channel write blocks indefinitely",
		ErrorLog:         "panic: deadlock",
		IdempotencyKey:   "idemp-key-001",
	}

	// 1. First submission -> Create Run & OutboxEvent
	run1, created1, err := diagSvc.Create(ctx, input)
	if err != nil || !created1 {
		t.Fatalf("first creation failed: %v", err)
	}
	if run1.Status != diagnosis.StatusQueued {
		t.Errorf("expected status QUEUED, got %s", run1.Status)
	}

	// Verification: DiagnosisAttempt count MUST be 0 during API acceptance phase
	attempts, err := diagStore.ListAttemptsByRun(ctx, run1.ID)
	if err != nil || len(attempts) != 0 {
		t.Fatalf("expected 0 attempts at API creation stage, got %d", len(attempts))
	}

	// 2. Duplicate submission with SAME request payload -> Return existing Run
	run2, created2, err := diagSvc.Create(ctx, input)
	if err != nil || created2 {
		t.Fatalf("expected duplicate recognized, but created2=%v (err=%v)", created2, err)
	}
	if run2.ID != run1.ID {
		t.Errorf("expected returned run ID %s to match %s", run2.ID, run1.ID)
	}

	// 3. Conflict submission with DIFFERENT request payload -> Must return 409 Conflict
	conflictInput := input
	conflictInput.IssueTitle = "DIFFERENT PAYLOAD TITLE"
	_, _, err3 := diagSvc.Create(ctx, conflictInput)
	if !errors.Is(err3, diagnosis.ErrIdempotencyConflict) {
		t.Fatalf("expected ErrIdempotencyConflict, got: %v", err3)
	}
}

// 2. Test Concurrent Worker Claim with Conditional Update and Fencing
func TestConcurrentWorkerClaimFencing(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-1",
		RepositoryID:           "repo-1",
		SnapshotID:             "snap-1",
		IssueTitle:             "Concurrent Claim Test",
		IdempotencyKey:         "k-claim-race",
		IdempotencyRequestHash: "h-claim-race",
	}
	_ = diagStore.CreateWithOutbox(ctx, run, nil)

	// Simulate 10 workers concurrently attempting to Claim the same Run
	numWorkers := 10
	var successfulClaims int64
	var claimConflicts int64

	var wg sync.WaitGroup
	expectedStatuses := []diagnosis.RunStatus{diagnosis.StatusQueued}

	for i := 1; i <= numWorkers; i++ {
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			_, attempt, err := diagStore.ClaimRun(ctx, run.ID, expectedStatuses, workerID, 5*time.Minute)
			if err == nil && attempt != nil {
				atomic.AddInt64(&successfulClaims, 1)
			} else if errors.Is(err, diagnosis.ErrClaimConflict) {
				atomic.AddInt64(&claimConflicts, 1)
			}
		}(uuid.New().String())
	}
	wg.Wait()

	if successfulClaims != 1 {
		t.Fatalf("expected EXACTLY 1 worker to successfully claim run, got %d", successfulClaims)
	}
	if claimConflicts != int64(numWorkers-1) {
		t.Fatalf("expected %d workers to receive claim conflict, got %d", numWorkers-1, claimConflicts)
	}

	// Verify attempt created in DB
	attempts, err := diagStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("expected exactly 1 attempt in DB, got %d", len(attempts))
	}
	if attempts[0].AttemptNo != 1 || attempts[0].Status != diagnosis.AttemptStatusRunning {
		t.Errorf("expected Attempt #1 in RUNNING status, got %+v", attempts[0])
	}
}

// 3. Test Outbox Relay -> Broker Pipeline
func TestTransactionalOutboxRelayPipeline(t *testing.T) {
	db := setupTestDB(t)
	broker := mq.NewMemoryBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagStore := diagnosis.NewStore(db)
	outboxStore := outbox.NewStore(db)

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-relay",
		RepositoryID:           "repo-relay",
		SnapshotID:             "snap-relay",
		IssueTitle:             "Outbox Relay Test",
		IdempotencyKey:         "k-relay",
		IdempotencyRequestHash: "h-relay",
	}

	// 1. Transactionally write DiagnosisRun + OutboxEvent
	outboxEvt := &outbox.OutboxEvent{}
	err := diagStore.CreateWithOutbox(ctx, run, outboxEvt)
	if err != nil {
		t.Fatalf("failed CreateWithOutbox: %v", err)
	}

	// Verify event is in PENDING status in DB
	pending, err := outboxStore.FetchPending(ctx, 10)
	if err != nil || len(pending) == 0 {
		t.Fatalf("expected pending outbox event in DB, got %d", len(pending))
	}

	// 2. Start Relay Daemon
	relay := outbox.NewRelay(outboxStore, broker, 50*time.Millisecond, 10)
	go relay.Start(ctx)

	// Consume from queue to verify delivery
	msgCh, err := broker.Consume(ctx, mq.QueueDiagnosisTask, 5)
	if err != nil {
		t.Fatalf("failed to consume from broker: %v", err)
	}

	select {
	case msg := <-msgCh:
		if msg.EventType != outbox.EventDiagnosisRequested {
			t.Errorf("expected event type %s, got %s", outbox.EventDiagnosisRequested, msg.EventType)
		}
		var payload worker.DiagnosisPayload
		if err := json.Unmarshal([]byte(msg.Payload), &payload); err != nil {
			t.Fatalf("failed to parse message payload: %v", err)
		}
		if payload.DiagnosisRunID != run.ID {
			t.Errorf("expected payload run ID %s, got %s", run.ID, payload.DiagnosisRunID)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for Outbox Relay to publish message to broker")
	}

	// Verify outbox event is marked as PUBLISHED in DB
	time.Sleep(100 * time.Millisecond)
	pendingAfter, _ := outboxStore.FetchPending(ctx, 10)
	if len(pendingAfter) != 0 {
		t.Errorf("expected 0 pending events after relay processing, got %d", len(pendingAfter))
	}
}

// 4. Test Worker Crash & Stale Attempt Recovery Sweeper
func TestWorkerCrashRecoveryAndStaleAttempt(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)
	outboxStore := outbox.NewStore(db)

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-crash",
		RepositoryID:           "repo-crash",
		SnapshotID:             "snap-crash",
		IssueTitle:             "Crash Recovery Simulation",
		IdempotencyKey:         "k-crash",
		IdempotencyRequestHash: "h-crash",
	}
	_ = diagStore.CreateWithOutbox(ctx, run, nil)

	// Worker 1 claims run -> Attempt #1
	_, att1, err := diagStore.ClaimRun(ctx, run.ID, []diagnosis.RunStatus{diagnosis.StatusQueued}, "worker-crashed-node", 5*time.Minute)
	if err != nil {
		t.Fatalf("claim failed: %v", err)
	}

	// Simulate Worker crash by setting heartbeat in past (expired)
	staleHeartbeat := time.Now().Add(-2 * time.Minute)
	_ = db.Model(&diagnosis.DiagnosisAttempt{}).Where("id = ?", att1.ID).Update("heartbeat_at", staleHeartbeat)

	// Run Recovery Sweeper
	sweeper := worker.NewRecoverySweeper(diagStore, 30*time.Second, 10*time.Millisecond, 50*time.Millisecond)
	recovered := sweeper.SweepOnce(ctx)
	if recovered != 1 {
		t.Fatalf("expected 1 recovered stale attempt, got %d", recovered)
	}

	// Verify Attempt #1 marked as ABANDONED
	att1Refreshed, _ := diagStore.GetAttempt(ctx, att1.ID)
	if att1Refreshed.Status != diagnosis.AttemptStatusAbandoned {
		t.Errorf("expected Attempt #1 ABANDONED, got %s", att1Refreshed.Status)
	}

	// Verify Run status is RETRY_WAIT
	runRefreshed, _ := diagStore.GetByID(ctx, run.ID)
	if runRefreshed.Status != diagnosis.StatusRetryWait {
		t.Errorf("expected Run status RETRY_WAIT, got %s", runRefreshed.Status)
	}

	// Verify Retry OutboxEvent was created
	time.Sleep(100 * time.Millisecond)
	pending, _ := outboxStore.FetchPending(ctx, 10)
	if len(pending) == 0 || pending[0].EventType != outbox.EventDiagnosisRetryRequested {
		t.Fatalf("expected DIAGNOSIS_RETRY_REQUESTED outbox event, got %+v", pending)
	}

	// Worker 2 claims run on retry -> Attempt #2 spawned!
	claimedRun2, att2, err := diagStore.ClaimRun(ctx, run.ID, []diagnosis.RunStatus{diagnosis.StatusRetryWait}, "worker-healthy-node", 5*time.Minute)
	if err != nil || att2 == nil {
		t.Fatalf("second claim failed: %v", err)
	}
	if att2.AttemptNo != 2 {
		t.Errorf("expected AttemptNo 2, got %d", att2.AttemptNo)
	}
	if claimedRun2.Status != diagnosis.StatusRunning {
		t.Errorf("expected Run RUNNING, got %s", claimedRun2.Status)
	}
}

// 5. Test Duplicate Delivery & ACK Loss Idempotency
func TestDuplicateDeliveryIdempotency(t *testing.T) {
	db := setupTestDB(t)
	broker := mq.NewMemoryBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore("/tmp/repolens_dup_test")
	citVal := evidence.NewCitationValidator(storeFS)

	var executionCount int64
	mockExecutor := &mockCountingExecutor{counter: &executionCount}

	consumer := worker.NewDiagnosisConsumer(
		worker.ConsumerConfig{
			WorkerID:        "worker-dup-test",
			Prefetch:        2,
			MaxAttempts:     3,
			AttemptDeadline: 2 * time.Minute,
			RetryBackoff:    50 * time.Millisecond,
		},
		broker,
		diagStore,
		repStore,
		citStore,
		citVal,
		mockExecutor,
		nil,
	)

	go func() {
		_ = consumer.Start(ctx)
	}()

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-dup",
		RepositoryID:           "repo-dup",
		SnapshotID:             "snap-dup",
		IssueTitle:             "Duplicate Delivery Bug",
		IdempotencyKey:         "k-dup",
		IdempotencyRequestHash: "h-dup",
	}
	_ = diagStore.CreateWithOutbox(ctx, run, nil)

	payloadBytes, _ := json.Marshal(worker.DiagnosisPayload{
		DiagnosisRunID: run.ID,
		RepositoryID:   run.RepositoryID,
		SnapshotID:     run.SnapshotID,
		UserID:         run.UserID,
	})

	// Message 1: First delivery
	_ = broker.Publish(ctx, mq.QueueDiagnosisTask, mq.Message{
		ID:        "msg-1",
		EventType: outbox.EventDiagnosisRequested,
		Payload:   string(payloadBytes),
	})

	time.Sleep(150 * time.Millisecond)

	// Message 2: Duplicate delivery (e.g. RabbitMQ redelivery on ACK loss)
	_ = broker.Publish(ctx, mq.QueueDiagnosisTask, mq.Message{
		ID:        "msg-1-dup",
		EventType: outbox.EventDiagnosisRequested,
		Payload:   string(payloadBytes),
	})

	time.Sleep(150 * time.Millisecond)

	if atomic.LoadInt64(&executionCount) != 1 {
		t.Fatalf("expected executor called EXACTLY once due to consumer idempotency, got %d", executionCount)
	}

	finalRun, _ := diagStore.GetByID(ctx, run.ID)
	if finalRun.Status != diagnosis.StatusSucceeded {
		t.Errorf("expected final status SUCCEEDED, got %s", finalRun.Status)
	}
}

// 6. Test DLQ Routing on Poison Messages
func TestRabbitMQDLQRoutingOnPoisonMessage(t *testing.T) {
	db := setupTestDB(t)
	broker := mq.NewMemoryBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore("/tmp/repolens_dlq_test")
	citVal := evidence.NewCitationValidator(storeFS)
	mockExecutor := worker.NewFakeDiagnosisExecutor()

	consumer := worker.NewDiagnosisConsumer(
		worker.ConsumerConfig{
			WorkerID:        "worker-dlq-test",
			Prefetch:        2,
			MaxAttempts:     3,
			AttemptDeadline: 2 * time.Minute,
		},
		broker,
		diagStore,
		repStore,
		citStore,
		citVal,
		mockExecutor,
		nil,
	)

	go func() {
		_ = consumer.Start(ctx)
	}()

	// Publish malformed poison message
	poisonMsg := mq.Message{
		ID:        "msg-poison-001",
		EventType: "UNKNOWN_MALFORMED",
		Payload:   "invalid-json-content{{{",
	}
	_ = broker.Publish(ctx, mq.QueueDiagnosisTask, poisonMsg)

	// Consume from DLQ queue to verify it was routed to DLQ
	dlqCh, err := broker.Consume(ctx, mq.QueueDiagnosisDLQ, 5)
	if err != nil {
		t.Fatalf("failed to consume DLQ: %v", err)
	}

	select {
	case dlqMsg := <-dlqCh:
		if dlqMsg.ID != poisonMsg.ID {
			t.Errorf("expected DLQ message ID %s, got %s", poisonMsg.ID, dlqMsg.ID)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for poison message to be routed to DLQ")
	}
}

// 7. Test Elasticsearch Bulk Indexing & Hybrid RRF
func TestElasticsearchAndRRFIntegration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodHead:
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && (r.URL.Path == "/_bulk" || r.URL.Path == "/repolens_chunks/_bulk"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errors":false,"items":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repolens_chunks/_search":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{
				"hits": {
					"total": {"value": 1},
					"hits": [
						{
							"_id": "c10",
							"_score": 3.8,
							"_source": {
								"snapshot_id": "snap-es-int",
								"chunk_id": "c10",
								"path": "internal/indexing/filter.go",
								"language": "go",
								"symbol": "NewFileFilter",
								"start_line": 1,
								"end_line": 20,
								"content": "func NewFileFilter() *FileFilter {}"
							}
						}
					]
				}
			}`))
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	esClient := elasticsearch.NewClient(server.URL, "repolens_chunks")
	ctx := context.Background()

	// 1. Bulk Indexing
	chunks := []indexing.CodeChunk{
		{
			ID:         "c10",
			SnapshotID: "snap-es-int",
			Path:       "internal/indexing/filter.go",
			Language:   "go",
			Symbol:     "NewFileFilter",
			StartLine:  1,
			EndLine:    20,
			Content:    "func NewFileFilter() *FileFilter {}",
		},
	}
	embedder := retrieval.NewLocalTFIDFEmbeddingProvider(128)
	vecs, _ := embedder.Embed(ctx, []string{chunks[0].Content})

	if err := esClient.BulkIndexChunks(ctx, chunks, vecs); err != nil {
		t.Fatalf("failed to bulk index into ES: %v", err)
	}

	// 2. Hybrid RRF Retrieval
	esBM25 := retrieval.NewESBM25Retriever(esClient)
	esVector := retrieval.NewESVectorRetriever(esClient, embedder)
	hybrid := retrieval.NewHybridRRFRetriever(60, esBM25, esVector)

	res, err := hybrid.Search(ctx, retrieval.SearchRequest{
		SnapshotID: "snap-es-int",
		Query:      "file filter",
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("hybrid search failed: %v", err)
	}
	if len(res) == 0 || res[0].Path != "internal/indexing/filter.go" {
		t.Errorf("unexpected hybrid search result: %+v", res)
	}
}

type mockCountingExecutor struct {
	counter *int64
}

func (m *mockCountingExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	atomic.AddInt64(m.counter, 1)
	return &worker.ExecutionResult{
		Report: &evidence.DiagnosisReportData{
			Summary:   "Mock diagnostic summary",
			RootCause: "Identified mock root cause",
		},
		PromptTokens:     100,
		CompletionTokens: 150,
		ToolCalls:        1,
	}, nil
}

// 8. Test Application Retry on 429 Rate Limit
type mockRateLimitThenSuccessExecutor struct {
	attempts int64
}

func (m *mockRateLimitThenSuccessExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	curr := atomic.AddInt64(&m.attempts, 1)
	if curr == 1 {
		return &worker.ExecutionResult{
			Retryable:    true,
			ErrorCode:    "RATE_LIMIT_429",
			ErrorMessage: "rate limit 429: backoff required",
		}, errors.New("rate limit 429 from LLM API")
	}
	return &worker.ExecutionResult{
		Report: &evidence.DiagnosisReportData{
			Summary:   "Success on retry attempt",
			RootCause: "Identified root cause on Attempt #2",
		},
		PromptTokens:     120,
		CompletionTokens: 200,
	}, nil
}

func TestApplicationRetryOn429RateLimit(t *testing.T) {
	db := setupTestDB(t)
	broker := mq.NewMemoryBroker()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagStore := diagnosis.NewStore(db)
	outboxStore := outbox.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore("/tmp/repolens_retry_test")
	citVal := evidence.NewCitationValidator(storeFS)

	exec := &mockRateLimitThenSuccessExecutor{}
	consumer := worker.NewDiagnosisConsumer(
		worker.ConsumerConfig{
			WorkerID:        "worker-retry-test",
			Prefetch:        2,
			MaxAttempts:     3,
			AttemptDeadline: 2 * time.Minute,
			RetryBackoff:    50 * time.Millisecond,
		},
		broker,
		diagStore,
		repStore,
		citStore,
		citVal,
		exec,
		nil,
	)

	go func() {
		_ = consumer.Start(ctx)
	}()

	// Start relay for delayed outbox events
	relay := outbox.NewRelay(outboxStore, broker, 30*time.Millisecond, 10)
	go relay.Start(ctx)

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-retry-429",
		RepositoryID:           "repo-retry-429",
		SnapshotID:             "snap-retry-429",
		IssueTitle:             "429 Retry Test",
		IdempotencyKey:         "k-retry-429",
		IdempotencyRequestHash: "h-retry-429",
	}
	_ = diagStore.CreateWithOutbox(ctx, run, nil)

	payloadBytes, _ := json.Marshal(worker.DiagnosisPayload{
		DiagnosisRunID: run.ID,
		RepositoryID:   run.RepositoryID,
		SnapshotID:     run.SnapshotID,
		UserID:         run.UserID,
	})

	// Publish first attempt message
	_ = broker.Publish(ctx, mq.QueueDiagnosisTask, mq.Message{
		ID:        "msg-retry-1",
		EventType: outbox.EventDiagnosisRequested,
		Payload:   string(payloadBytes),
	})

	// Wait for Attempt #1 failure -> RetryWait -> Relay publishes Retry event -> Attempt #2 success
	time.Sleep(350 * time.Millisecond)

	attempts, err := diagStore.ListAttemptsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list attempts: %v", err)
	}
	if len(attempts) < 2 {
		t.Fatalf("expected at least 2 attempts (1 failed_retryable + 1 succeeded), got %d", len(attempts))
	}
	if attempts[0].Status != diagnosis.AttemptStatusFailedRetryable {
		t.Errorf("expected Attempt #1 FAILED_RETRYABLE, got %s", attempts[0].Status)
	}
	if attempts[1].Status != diagnosis.AttemptStatusSucceeded {
		t.Errorf("expected Attempt #2 SUCCEEDED, got %s", attempts[1].Status)
	}

	finalRun, _ := diagStore.GetByID(ctx, run.ID)
	if finalRun.Status != diagnosis.StatusSucceeded {
		t.Errorf("expected final run status SUCCEEDED, got %s", finalRun.Status)
	}
}
