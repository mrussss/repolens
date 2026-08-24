package integration_real

import (
	"context"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	tc "github.com/testcontainers/testcontainers-go"
	tcrabbitmq "github.com/testcontainers/testcontainers-go/modules/rabbitmq"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/mq"
	"repolens/internal/outbox"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/worker"
)

func setupRealRabbitMQ(t *testing.T) (mq.Broker, string, func()) {
	ctx := context.Background()

	rmqContainer, err := tcrabbitmq.RunContainer(ctx,
		tc.WithImage("rabbitmq:3.12-management"),
	)
	if err != nil {
		t.Skipf("Skipping real RabbitMQ testcontainers test (Docker not available: %v)", err)
		return nil, "", nil
	}

	amqpURL, err := rmqContainer.AmqpURL(ctx)
	if err != nil {
		_ = rmqContainer.Terminate(ctx)
		t.Fatalf("failed to get AMQP URL: %v", err)
	}

	broker, err := mq.NewRabbitMQBroker(amqpURL)
	if err != nil {
		_ = rmqContainer.Terminate(ctx)
		t.Fatalf("failed to connect to real RabbitMQ broker: %v", err)
	}

	cleanup := func() {
		_ = broker.Close()
		_ = rmqContainer.Terminate(context.Background())
	}

	return broker, amqpURL, cleanup
}

func setupIsolatedSQLiteDB(t *testing.T) *gorm.DB {
	dbName := fmt.Sprintf("file:rmq_mem_%s?mode=memory&cache=shared", uuid.New().String())
	db, err := gorm.Open(sqlite.Open(dbName), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open test sqlite DB: %v", err)
	}
	sqlDB, err := db.DB()
	if err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed to migrate test DB: %v", err)
	}
	return db
}

type countingExecutor struct {
	counter *int64
}

func (m *countingExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*worker.ExecutionResult, error) {
	atomic.AddInt64(m.counter, 1)
	return &worker.ExecutionResult{
		Report: &evidence.DiagnosisReportData{
			Summary:   "Verified root cause report from real RabbitMQ execution",
			RootCause: "Identified bug in memory pool allocation",
		},
		PromptTokens:     100,
		CompletionTokens: 150,
		ToolCalls:        1,
	}, nil
}

func TestRealRabbitMQ_OutboxRelayPipeline(t *testing.T) {
	broker, _, cleanup := setupRealRabbitMQ(t)
	if broker == nil {
		return
	}
	defer cleanup()

	db := setupIsolatedSQLiteDB(t)
	diagStore := diagnosis.NewStore(db)
	outboxStore := outbox.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore("/tmp/repolens_rmq_test")
	citVal := evidence.NewCitationValidator(storeFS)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var execCount int64
	exec := &countingExecutor{counter: &execCount}

	consumer := worker.NewDiagnosisConsumer(
		worker.ConsumerConfig{
			WorkerID:        "worker-real-rmq",
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
		exec,
		nil,
	)
	go func() {
		_ = consumer.Start(ctx)
	}()

	relay := outbox.NewRelay(outboxStore, broker, 50*time.Millisecond, 10)
	go relay.Start(ctx)

	// Create diagnosis run and outbox event
	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-rmq-01",
		RepositoryID:           "repo-rmq-01",
		SnapshotID:             "snap-rmq-01",
		IssueTitle:             "Real RabbitMQ Outbox Relay Test",
		IdempotencyKey:         "k-rmq-01",
		IdempotencyRequestHash: "h-rmq-01",
	}
	outboxEvt := &outbox.OutboxEvent{}
	if err := diagStore.CreateWithOutbox(ctx, run, outboxEvt); err != nil {
		t.Fatalf("failed to create run with outbox: %v", err)
	}

	// Wait for Relay -> RabbitMQ -> Consumer -> Succeeded
	time.Sleep(600 * time.Millisecond)

	finalRun, err := diagStore.GetByID(ctx, run.ID)
	if err != nil || finalRun.Status != diagnosis.StatusSucceeded {
		t.Fatalf("expected run SUCCEEDED via real RabbitMQ, got %s (err: %v)", finalRun.Status, err)
	}

	if atomic.LoadInt64(&execCount) != 1 {
		t.Errorf("expected exactly 1 execution, got %d", atomic.LoadInt64(&execCount))
	}
}

func TestRealRabbitMQ_DuplicateDelivery(t *testing.T) {
	broker, _, cleanup := setupRealRabbitMQ(t)
	if broker == nil {
		return
	}
	defer cleanup()

	db := setupIsolatedSQLiteDB(t)
	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore("/tmp/repolens_rmq_dup_test")
	citVal := evidence.NewCitationValidator(storeFS)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var execCount int64
	exec := &countingExecutor{counter: &execCount}

	consumer := worker.NewDiagnosisConsumer(
		worker.ConsumerConfig{
			WorkerID:        "worker-dup-rmq",
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
		exec,
		nil,
	)
	go func() {
		_ = consumer.Start(ctx)
	}()

	run := &diagnosis.DiagnosisRun{
		UserID:                 "user-dup-rmq",
		RepositoryID:           "repo-dup-rmq",
		SnapshotID:             "snap-dup-rmq",
		IssueTitle:             "Duplicate Delivery RMQ Test",
		IdempotencyKey:         "k-dup-rmq",
		IdempotencyRequestHash: "h-dup-rmq",
	}
	_ = diagStore.CreateWithOutbox(ctx, run, nil)

	payloadBytes, _ := json.Marshal(worker.DiagnosisPayload{
		DiagnosisRunID: run.ID,
		RepositoryID:   run.RepositoryID,
		SnapshotID:     run.SnapshotID,
		UserID:         run.UserID,
	})

	// Publish First message to real RabbitMQ
	_ = broker.Publish(ctx, mq.QueueDiagnosisTask, mq.Message{
		ID:        "msg-dup-1",
		EventType: outbox.EventDiagnosisRequested,
		Payload:   string(payloadBytes),
	})

	// Wait for execution to finish
	time.Sleep(400 * time.Millisecond)

	if atomic.LoadInt64(&execCount) != 1 {
		t.Fatalf("first message should have executed once, got %d", atomic.LoadInt64(&execCount))
	}

	// Publish DUPLICATE message to real RabbitMQ
	_ = broker.Publish(ctx, mq.QueueDiagnosisTask, mq.Message{
		ID:          "msg-dup-2",
		EventType:   outbox.EventDiagnosisRequested,
		Payload:     string(payloadBytes),
		Redelivered: true,
	})

	// Wait for second message to be processed and safely ACKed
	time.Sleep(400 * time.Millisecond)

	if atomic.LoadInt64(&execCount) != 1 {
		t.Fatalf("duplicate message caused re-execution! Expected 1 execution, got %d", atomic.LoadInt64(&execCount))
	}
}

func TestRealRabbitMQ_PoisonMessageDLQ(t *testing.T) {
	broker, _, cleanup := setupRealRabbitMQ(t)
	if broker == nil {
		return
	}
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db := setupIsolatedSQLiteDB(t)
	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	storeFS := snapshotstore.NewLocalSnapshotStore("/tmp/repolens_rmq_dlq_test")
	citVal := evidence.NewCitationValidator(storeFS)

	exec := &countingExecutor{counter: new(int64)}
	consumer := worker.NewDiagnosisConsumer(
		worker.ConsumerConfig{
			WorkerID:        "worker-dlq-rmq",
			Prefetch:        1,
			MaxAttempts:     3,
			AttemptDeadline: 2 * time.Minute,
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

	// Publish malformed poison JSON to real RabbitMQ
	_ = broker.Publish(ctx, mq.QueueDiagnosisTask, mq.Message{
		ID:        "msg-poison-rmq-001",
		EventType: "UNKNOWN_EVENT",
		Payload:   "invalid-malformed-json-payload",
	})

	// Wait for consumer to route to DLQ
	time.Sleep(400 * time.Millisecond)

	// Consume from real DLQ to verify message arrived
	dlqChan, err := broker.Consume(ctx, mq.QueueDiagnosisDLQ, 1)
	if err != nil {
		t.Fatalf("failed to consume from DLQ on RabbitMQ: %v", err)
	}

	select {
	case dlqMsg := <-dlqChan:
		if dlqMsg.Payload != "invalid-malformed-json-payload" {
			t.Errorf("unexpected DLQ payload: %s", dlqMsg.Payload)
		}
		if dlqMsg.Headers["x-death-reason"] == "" {
			t.Errorf("expected x-death-reason header on DLQ message")
		}
		if dlqMsg.AckFunc != nil {
			_ = dlqMsg.AckFunc()
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for poison message in real RabbitMQ DLQ")
	}
}
