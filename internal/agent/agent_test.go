package agent_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/agent"
	"repolens/internal/diagnosis"
	"repolens/internal/llm"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval"
	"repolens/internal/tools"
	"repolens/internal/trace"
)

func setupTraceDB(t *testing.T) trace.Store {
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed to auto migrate: %v", err)
	}
	return trace.NewStore(db)
}

func TestAgentGuardRepeatCallDetection(t *testing.T) {
	guard := agent.NewAgentGuard(agent.GuardConfig{
		MaxSteps:       5,
		MaxToolCalls:   5,
		MaxRepeatCalls: 2,
	})

	// First call -> OK
	if err := guard.RecordToolCall("search_code", `{"query":"error"}`); err != nil {
		t.Fatalf("first call failed: %v", err)
	}

	// Consecutive identical call 1 -> OK
	if err := guard.RecordToolCall("search_code", `{"query":"error"}`); err != nil {
		t.Fatalf("second identical call should not fail threshold yet: %v", err)
	}

	// Consecutive identical call 2 -> Must fail!
	if err := guard.RecordToolCall("search_code", `{"query":"error"}`); err == nil {
		t.Fatalf("expected error on third consecutive identical tool call")
	}
}

func TestAgentLoopExecutionWithToolCalling(t *testing.T) {
	traceStore := setupTraceDB(t)
	chunkStore := retrieval.NewMemoryChunkStore()
	retriever := retrieval.NewLexicalRetriever(chunkStore)
	storeFS := snapshotstore.NewLocalSnapshotStore("/tmp/repolens_agent_test")

	registry := agent.NewToolRegistry()
	registry.Register(tools.NewSearchCodeTool(retriever, "snap-agent-1"))
	registry.Register(tools.NewReadFileTool(storeFS, "repo-agent-1", "snap-agent-1"))
	registry.Register(tools.NewReadDocsTool(storeFS, "repo-agent-1", "snap-agent-1"))
	registry.Register(tools.NewReadCILogTool("Error: nil pointer in handler.go:25"))

	// Tool calling then structured output
	fakeProvider := llm.NewFakeProvider(llm.ModeToolCallThenDone)

	loop := agent.NewAgentLoop(
		fakeProvider,
		registry,
		traceStore,
		agent.DefaultGuardConfig(),
	)

	run := &diagnosis.DiagnosisRun{
		ID:               "run-agent-1",
		RepositoryID:     "repo-agent-1",
		SnapshotID:       "snap-agent-1",
		IssueTitle:       "Nil pointer crash",
		IssueDescription: "Server crashed with segfault",
		ErrorLog:         "Error: nil pointer in handler.go:25",
	}
	attempt := &diagnosis.DiagnosisAttempt{
		ID:             "att-agent-1",
		DiagnosisRunID: run.ID,
		AttemptNo:      1,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	result, err := loop.Run(ctx, run, attempt)
	if err != nil || result == nil {
		t.Fatalf("agent loop execution failed: %v", err)
	}

	if result.Report == nil || result.Report.RootCause == "" {
		t.Fatalf("expected parsed report with root cause")
	}
	if result.ToolCallsCount < 1 {
		t.Errorf("expected at least 1 tool call executed, got %d", result.ToolCallsCount)
	}

	// Verify trace steps recorded in DB
	steps, err := traceStore.ListByAttempt(ctx, attempt.ID)
	if err != nil || len(steps) == 0 {
		t.Fatalf("expected trace steps persisted in DB, got %d (err: %v)", len(steps), err)
	}
}

func TestSecretRedaction(t *testing.T) {
	input := "Error occurred with api_key='sk-abcdef1234567890abcdef1234567890' and Authorization: Bearer ghp_123456789012345678901234567890123456"
	redacted := diagnosis.RedactSecrets(input)

	if strings.Contains(redacted, "sk-abcdef") || strings.Contains(redacted, "ghp_1234") {
		t.Errorf("secret not properly redacted: %s", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED_SECRET]") {
		t.Errorf("expected [REDACTED_SECRET] tag in output: %s", redacted)
	}
}
