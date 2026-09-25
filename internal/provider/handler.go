package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/snapshot"
	"repolens/internal/trace"

	"gorm.io/gorm"
)

var (
	errProviderConfigInUse       = errors.New("provider identity is pinned by an active diagnosis")
	errProviderActiveCheckFailed = errors.New("failed checking active diagnoses")
)

// Handler handles system provider configuration and demo mode endpoints.
type Handler struct {
	mgr             *Manager
	repoStore       repo.Store
	snapshotStore   snapshot.Store
	diagnosisStore  diagnosis.Store
	reportStore     evidence.ReportStore
	citationStore   evidence.CitationStore
	traceStore      trace.Store
	storeFS         snapshotstore.SnapshotStore
	db              *gorm.DB
	codeIntelStore  codeintelstore.Store
	indexStorageDir string
}

// WithDemoDependencies enables the real local code-intelligence demo path.
func (h *Handler) WithDemoDependencies(db *gorm.DB, codeIntelStore codeintelstore.Store, indexStorageDir string) *Handler {
	h.db, h.codeIntelStore, h.indexStorageDir = db, codeIntelStore, indexStorageDir
	return h
}

// NewHandler constructs a new Handler.
func NewHandler(
	mgr *Manager,
	repoStore repo.Store,
	snapshotStore snapshot.Store,
	diagnosisStore diagnosis.Store,
	reportStore evidence.ReportStore,
	citationStore evidence.CitationStore,
	traceStore trace.Store,
	storeFS snapshotstore.SnapshotStore,
) *Handler {
	return &Handler{
		mgr:            mgr,
		repoStore:      repoStore,
		snapshotStore:  snapshotStore,
		diagnosisStore: diagnosisStore,
		reportStore:    reportStore,
		citationStore:  citationStore,
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
	BaseURL  string `json:"base_url" binding:"required"`
	Model    string `json:"model" binding:"required"`
	APIKey   string `json:"api_key"`
	AuthMode string `json:"auth_mode"`
}

// SaveConfig saves new provider credentials.
func (h *Handler) SaveConfig(c *gin.Context) {
	var req SaveProviderRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PROVIDER_CONFIG", "error": "invalid provider configuration"})
		return
	}

	newBase, err := NormalizeBaseURL(req.BaseURL)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":  "INVALID_PROVIDER_CONFIG",
			"error": "invalid provider configuration",
		})
		return
	}
	newConfigFingerprint := ComputeConfigFingerprint(newBase, req.Model, normalizeAuthMode(req.AuthMode))
	err = h.withProviderConfigLock(c.Request.Context(), func() error {
		current := h.mgr.GetPublicStatus()
		identityChanged := current.ConfigFingerprint == "" || newConfigFingerprint != current.ConfigFingerprint
		if h.diagnosisStore != nil && identityChanged {
			hasActive, checkErr := h.diagnosisStore.HasActiveRuns(c.Request.Context())
			if checkErr != nil {
				return fmt.Errorf("%w: %v", errProviderActiveCheckFailed, checkErr)
			}
			if hasActive {
				return errProviderConfigInUse
			}
		}
		return h.mgr.SaveConfigWithAuthMode(newBase, req.Model, req.APIKey, req.AuthMode, false)
	})
	if err != nil {
		if errors.Is(err, errProviderConfigInUse) {
			c.JSON(http.StatusConflict, gin.H{"code": "PROVIDER_CONFIG_IN_USE", "error": "provider identity is pinned by an active diagnosis"})
			return
		}
		if errors.Is(err, errProviderActiveCheckFailed) {
			logger.L(c.Request.Context()).Error("failed checking active diagnoses before provider identity change", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": "PROVIDER_ACTIVE_RUN_CHECK_FAILED", "error": "failed to verify active diagnoses"})
			return
		}
		if errors.Is(err, ErrInvalidProviderConfig) {
			c.JSON(http.StatusBadRequest, gin.H{
				"code":  "INVALID_PROVIDER_CONFIG",
				"error": "invalid provider configuration",
			})
			return
		}
		logger.L(c.Request.Context()).Error("failed to save provider configuration", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":  "PROVIDER_CONFIG_SAVE_FAILED",
			"error": "failed to save provider configuration",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "provider configuration saved successfully",
		"status":  h.mgr.GetPublicStatus(),
	})
}

func (h *Handler) ClearConfig(c *gin.Context) {
	err := h.withProviderConfigLock(c.Request.Context(), func() error {
		if h.diagnosisStore != nil {
			hasActive, checkErr := h.diagnosisStore.HasActiveRuns(c.Request.Context())
			if checkErr != nil {
				return fmt.Errorf("%w: %v", errProviderActiveCheckFailed, checkErr)
			}
			if hasActive {
				return errProviderConfigInUse
			}
		}
		return h.mgr.ClearConfig()
	})
	if err != nil {
		if errors.Is(err, errProviderConfigInUse) {
			c.JSON(http.StatusConflict, gin.H{"code": "PROVIDER_CONFIG_IN_USE", "error": "provider identity is pinned by an active diagnosis"})
			return
		}
		if errors.Is(err, errProviderActiveCheckFailed) {
			logger.L(c.Request.Context()).Error("failed checking active diagnoses before clearing provider configuration", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": "PROVIDER_ACTIVE_RUN_CHECK_FAILED", "error": "failed to verify active diagnoses"})
			return
		}
		logger.L(c.Request.Context()).Error("failed to clear provider configuration", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":  "PROVIDER_CONFIG_CLEAR_FAILED",
			"error": "failed to clear provider configuration",
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "provider configuration cleared", "status": h.mgr.GetPublicStatus()})
}

func (h *Handler) withProviderConfigLock(ctx context.Context, fn func() error) error {
	if h.diagnosisStore != nil {
		return h.diagnosisStore.WithProviderConfigLock(ctx, fn)
	}
	return fn()
}

type TestConnectionRequest struct {
	BaseURL  string `json:"base_url" binding:"required"`
	Model    string `json:"model" binding:"required"`
	APIKey   string `json:"api_key"`
	AuthMode string `json:"auth_mode"`
}

// TestConnection verifies connectivity with the target LLM provider.
func (h *Handler) TestConnection(c *gin.Context) {
	var req TestConnectionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_PROVIDER_CONFIG", "error": "invalid provider configuration"})
		return
	}
	if _, err := NormalizeBaseURL(req.BaseURL); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"success": false,
			"code":    "INVALID_PROVIDER_CONFIG",
			"error":   "invalid provider configuration",
		})
		return
	}

	latency, compatibility, err := h.mgr.TestConnectionCompatibilityWithAuthMode(c.Request.Context(), req.BaseURL, req.Model, req.APIKey, req.AuthMode)
	if err != nil {
		code, message, status := ClassifyTestConnectionError(err)
		// Do not log the wrapped provider error: it may contain upstream body
		// text or request details. The stable code is sufficient for operations.
		logger.L(c.Request.Context()).Error("provider connection test failed", "code", code)
		c.JSON(status, gin.H{
			"success":    false,
			"code":       code,
			"error":      message,
			"latency_ms": latency.Milliseconds(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":       true,
		"latency_ms":    latency.Milliseconds(),
		"message":       fmt.Sprintf("Connection successful! Latency: %dms", latency.Milliseconds()),
		"compatibility": compatibility,
	})
}

func writeProviderInternalError(c *gin.Context, code, message string, err error) {
	logger.L(c.Request.Context()).Error(message, "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"code": code, "error": message})
}

// TriggerDemo creates a deterministic bundled demo repository, snapshot, and diagnosis run.
func (h *Handler) TriggerDemo(c *gin.Context) {
	if h.db != nil && h.codeIntelStore != nil {
		h.triggerRealDemo(c)
		return
	}
	ctx := c.Request.Context()
	userID := c.GetString("user_id")
	if userID == "" {
		userID = "local-user"
	}

	// 1. Ensure Demo Repository
	demoRepoID := "repo-demo-order-svc"
	existingRepo, _ := h.repoStore.GetByID(ctx, demoRepoID)
	if existingRepo == nil {
		demoRepo := &repo.Repository{
			ID:         demoRepoID,
			UserID:     userID,
			Name:       "order-service (Demo)",
			GitURL:     "https://github.com/repolens/demo-order-service",
			DefaultRef: "main",
			Status:     "ACTIVE",
		}
		_ = h.repoStore.Create(ctx, demoRepo)
	} else if existingRepo.UserID != userID {
		// The demo repository is a local singleton; keep it visible to the
		// current local user even when an older v1 database used demo-user.
		existingRepo.UserID = userID
		_ = h.repoStore.Update(ctx, existingRepo)
	}

	// 2. Ensure Demo Snapshot files
	demoSnapID := "snap-demo-order-svc-002"
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
		mainBytes, _ := os.ReadFile(mainFile)
		contentSum := sha256.Sum256(mainBytes)
		demoSnap := &snapshot.RepositorySnapshot{
			ID:               demoSnapID,
			RepositoryID:     demoRepoID,
			CommitSHA:        demoCommit,
			Ref:              "main",
			MaterializedPath: demoDir,
			Status:           snapshot.StatusReady,
			ContentHash:      hex.EncodeToString(contentSum[:]),
			FileCount:        1,
			TotalBytes:       int64(len(mainBytes)),
			ReadyAt:          &now,
		}
		_ = h.snapshotStore.Create(ctx, demoSnap)
	}

	// 3. Create Demo Diagnosis Run
	runID := "diag-demo-" + uuid.New().String()[:8]
	demoRun := &diagnosis.DiagnosisRun{
		ID:                     runID,
		UserID:                 userID,
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
		writeProviderInternalError(c, "DEMO_INITIALIZATION_FAILED", "failed to initialize demo", err)
		return
	}

	// 4. Create deterministic Demo Report & Citations
	attemptID := "attempt-demo-1"
	if starter, ok := h.diagnosisStore.(interface {
		StartAttempt(context.Context, string, *diagnosis.DiagnosisAttempt) error
	}); ok {
		attemptStartedAt := now
		if err := starter.StartAttempt(ctx, demoRun.ID, &diagnosis.DiagnosisAttempt{
			ID: attemptID, DiagnosisRunID: demoRun.ID, ExecutionGeneration: 1, AttemptNo: 1, WorkerID: "demo",
			StartedAt: attemptStartedAt, HeartbeatAt: attemptStartedAt, DeadlineAt: attemptStartedAt.Add(time.Hour),
		}); err != nil {
			writeProviderInternalError(c, "DEMO_INITIALIZATION_FAILED", "failed to initialize demo", err)
			return
		}
	}
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
	if h.citationStore != nil {
		demoCitation := evidence.Citation{
			ID: uuid.New().String(), ReportID: demoReport.ID, SnapshotID: demoSnapID,
			FilePath: "main.go", StartLine: 19, EndLine: 29, Reason: "Unbuffered channel initialization and blocking write",
			CreatedAt: now,
		}
		evidence.NewCitationValidator(h.storeFS).Validate(ctx, demoRepoID, demoSnapID, &demoCitation)
		_ = h.citationStore.CreateBatch(ctx, []evidence.Citation{demoCitation})
	}

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
