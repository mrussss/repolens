package provider

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
)

// Handler handles system provider configuration and demo mode endpoints.
type Handler struct {
	mgr            *Manager
	repoStore      repo.Store
	snapshotStore  snapshot.Store
	diagnosisStore diagnosis.Store
	reportStore    evidence.ReportStore
	traceStore     trace.Store
	storeFS        snapshotstore.SnapshotStore
}

// NewHandler constructs a new Handler.
func NewHandler(
	mgr *Manager,
	repoStore repo.Store,
	snapshotStore snapshot.Store,
	diagnosisStore diagnosis.Store,
	reportStore evidence.ReportStore,
	traceStore trace.Store,
	storeFS snapshotstore.SnapshotStore,
) *Handler {
	return &Handler{
		mgr:            mgr,
		repoStore:      repoStore,
		snapshotStore:  snapshotStore,
		diagnosisStore: diagnosisStore,
		reportStore:    reportStore,
		traceStore:     traceStore,
		storeFS:        storeFS,
	}
}

// GetStatus returns the current public provider configuration status.
func (h *Handler) GetStatus(c *gin.Context) {
	status := h.mgr.GetPublicStatus()
	c.JSON(http.StatusOK, status)
}

type SaveProviderRequest struct {
	BaseURL string `json:"base_url" binding:"required"`
	Model   string `json:"model" binding:"required"`
	APIKey  string `json:"api_key" binding:"required"`
}

// SaveConfig saves new provider credentials.
func (h *Handler) SaveConfig(c *gin.Context) {
	var req SaveProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.mgr.SaveConfig(req.BaseURL, req.Model, req.APIKey, false); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "provider configuration saved successfully",
		"status":  h.mgr.GetPublicStatus(),
	})
}

type TestConnectionRequest struct {
	BaseURL string `json:"base_url" binding:"required"`
	Model   string `json:"model" binding:"required"`
	APIKey  string `json:"api_key" binding:"required"`
}

// TestConnection verifies connectivity with the target LLM provider.
func (h *Handler) TestConnection(c *gin.Context) {
	var req TestConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	latency, err := h.mgr.TestConnection(c.Request.Context(), req.BaseURL, req.Model, req.APIKey)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"success":    false,
			"error":      err.Error(),
			"latency_ms": latency.Milliseconds(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":    true,
		"latency_ms": latency.Milliseconds(),
		"message":    fmt.Sprintf("Connection successful! Latency: %dms", latency.Milliseconds()),
	})
}

// TriggerDemo creates a deterministic bundled demo repository, snapshot, and diagnosis run.
func (h *Handler) TriggerDemo(c *gin.Context) {
	ctx := c.Request.Context()

	// 1. Ensure Demo Repository
	demoRepoID := "repo-demo-order-svc"
	existingRepo, _ := h.repoStore.GetByID(ctx, demoRepoID)
	if existingRepo == nil {
		demoRepo := &repo.Repository{
			ID:         demoRepoID,
			UserID:     "demo-user",
			Name:       "order-service (Demo)",
			GitURL:     "https://github.com/repolens/demo-order-service",
			DefaultRef: "main",
			Status:     "ACTIVE",
		}
		_ = h.repoStore.Create(ctx, demoRepo)
	}

	// 2. Ensure Demo Snapshot files
	demoSnapID := "snap-demo-order-svc-001"
	demoDir := h.storeFS.GetSourcePath(demoRepoID, demoSnapID)
	_ = os.MkdirAll(demoDir, 0755)

	mainFile := filepath.Join(demoDir, "main.go")
	_ = os.WriteFile(mainFile, []byte(`package main

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type OrderProcessor struct {
	jobsChan chan Order
	mu       sync.Mutex
	closed   bool
}

type Order struct {
	ID     string
	Amount float64
}

func NewOrderProcessor() *OrderProcessor {
	return &OrderProcessor{
		jobsChan: make(chan Order), // Unbuffered channel causes deadlock when worker pool is full
	}
}

func (p *OrderProcessor) SubmitOrder(ctx context.Context, order Order) error {
	p.jobsChan <- order // Bug: Blocks indefinitely if consumer is slow or unbuffered
	return nil
}
`), 0644)

	now := time.Now().UTC()
	existingSnap, _ := h.snapshotStore.GetByID(ctx, demoSnapID)
	if existingSnap == nil {
		demoSnap := &snapshot.RepositorySnapshot{
			ID:               demoSnapID,
			RepositoryID:     demoRepoID,
			CommitSHA:        "e4d3c2b1a09876543210",
			Ref:              "main",
			MaterializedPath: demoDir,
			Status:           snapshot.StatusReady,
			ReadyAt:          &now,
		}
		_ = h.snapshotStore.Create(ctx, demoSnap)
	}

	// 3. Create Demo Diagnosis Run
	runID := "diag-demo-" + uuid.New().String()[:8]
	demoRun := &diagnosis.DiagnosisRun{
		ID:                     runID,
		UserID:                 "demo-user",
		RepositoryID:           demoRepoID,
		SnapshotID:             demoSnapID,
		IssueTitle:             "[Demo] Order submission worker deadlock under load",
		IssueDescription:       "Under high concurrent order load, the HTTP handler hangs and stops accepting new orders after 100 requests.",
		ErrorLog:               "panic: deadlock detected in goroutine 42 [chan send]: main.(*OrderProcessor).SubmitOrder(0xc0000a0, {0x1234, 0x5}) main.go:27",
		Status:                 diagnosis.StatusSucceeded,
		IdempotencyKey:         "idemp-demo-" + uuid.New().String()[:8],
		IdempotencyRequestHash: "hash-demo",
		Version:                1,
	}

	if err := h.diagnosisStore.Create(ctx, demoRun); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed creating demo diagnosis: " + err.Error()})
		return
	}

	// 4. Create deterministic Demo Report & Citations
	attemptID := "attempt-demo-1"
	demoReport := &evidence.Report{
		ID:             "report-demo-" + uuid.New().String()[:8],
		DiagnosisRunID: demoRun.ID,
		AttemptID:      attemptID,
		RootCause:      "Unbuffered channel `jobsChan` in `NewOrderProcessor()` creates a synchronization bottleneck. In `SubmitOrder()`, sending to the unbuffered channel blocks indefinitely when all worker goroutines are busy, causing incoming HTTP request handlers to hang and eventually crash with a goroutine deadlock.",
		FindingsJSON: `[
			{
				"title": "Unbuffered channel deadlock in OrderProcessor",
				"reasoning": "The channel 'jobsChan' is initialized with make(chan Order) without buffer capacity. In SubmitOrder, writing to this channel requires an immediate receiver. Under concurrent load, sender goroutines block permanently.",
				"citations": [
					{
						"snapshot_id": "` + demoSnapID + `",
						"file_path": "main.go",
						"start_line": 19,
						"end_line": 29,
						"excerpt": "func NewOrderProcessor() *OrderProcessor {\n\treturn &OrderProcessor{\n\t\tjobsChan: make(chan Order),\n\t}\n}\n\nfunc (p *OrderProcessor) SubmitOrder(ctx context.Context, order Order) error {\n\tp.jobsChan <- order\n\treturn nil\n}",
						"reason": "Unbuffered channel initialization and blocking write"
					}
				]
			}
		]`,
		RecommendedChecksJSON: `[
			"Add capacity buffer to jobsChan, e.g. make(chan Order, 1000)",
			"Implement select with ctx.Done() or timeout fallback in SubmitOrder()",
			"Add integration load test to verify channel drain under high concurrency"
		]`,
		Confidence: 0.98,
		CreatedAt:  now,
	}
	_ = h.reportStore.Create(ctx, demoReport)

	// Add Trace Steps
	_ = h.traceStore.Create(ctx, &trace.AgentStep{
		ID:                uuid.New().String(),
		AttemptID:         attemptID,
		Seq:               1,
		StepType:          trace.StepTypeToolCall,
		ToolName:          "search_code",
		ToolArgsSummary:   `{"query": "OrderProcessor SubmitOrder jobsChan"}`,
		ToolResultSummary: `main.go:L19-29`,
		Status:            "COMPLETED",
		LatencyMs:         45,
		InputTokens:       250,
		OutputTokens:      60,
		CreatedAt:         now.Add(-2 * time.Second),
	})
	_ = h.traceStore.Create(ctx, &trace.AgentStep{
		ID:                uuid.New().String(),
		AttemptID:         attemptID,
		Seq:               2,
		StepType:          trace.StepTypeToolCall,
		ToolName:          "read_file",
		ToolArgsSummary:   `{"path": "main.go", "start_line": 1, "end_line": 35}`,
		ToolResultSummary: `32 lines read`,
		Status:            "COMPLETED",
		LatencyMs:         12,
		InputTokens:       180,
		OutputTokens:      320,
		CreatedAt:         now.Add(-1 * time.Second),
	})

	// Mark status succeeded
	_ = h.diagnosisStore.FinishAttemptAndRun(
		ctx, demoRun.ID, attemptID, diagnosis.StatusSucceeded, diagnosis.AttemptStatusSucceeded,
		430, 380, 2, "", "", false, 0,
	)

	c.JSON(http.StatusCreated, gin.H{
		"message":       "Demo environment and diagnosis initialized successfully",
		"diagnosis_id":  demoRun.ID,
		"repository_id": demoRepoID,
		"snapshot_id":   demoSnapID,
		"report":        demoReport,
	})
}
