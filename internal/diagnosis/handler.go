package diagnosis

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"repolens/internal/evidence"
	"repolens/internal/platform/logger"
	"repolens/internal/trace"
)

type Handler struct {
	svc           *Service
	reportStore   evidence.ReportStore
	citationStore evidence.CitationStore
	traceStore    trace.Store
}

func NewHandler(svc *Service, reportStore evidence.ReportStore, citationStore evidence.CitationStore, traceStore trace.Store) *Handler {
	return &Handler{
		svc:           svc,
		reportStore:   reportStore,
		citationStore: citationStore,
		traceStore:    traceStore,
	}
}

type CreateDiagnosisRequest struct {
	RepositoryID     string `json:"repository_id" binding:"required"`
	SnapshotID       string `json:"snapshot_id" binding:"required"`
	IssueTitle       string `json:"issue_title" binding:"required"`
	IssueDescription string `json:"issue_description"`
	ErrorLog         string `json:"error_log"`
	IdempotencyKey   string `json:"idempotency_key"`
}

func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	var req CreateDiagnosisRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	idempKey := c.GetHeader("Idempotency-Key")
	if idempKey == "" {
		idempKey = req.IdempotencyKey
	}

	input := CreateDiagnosisInput{
		UserID:           userID,
		RepositoryID:     req.RepositoryID,
		SnapshotID:       req.SnapshotID,
		IssueTitle:       req.IssueTitle,
		IssueDescription: req.IssueDescription,
		ErrorLog:         req.ErrorLog,
		IdempotencyKey:   idempKey,
	}

	run, created, err := h.svc.Create(c.Request.Context(), input)
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "Idempotency conflict: key reused with differing request payload"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if !created {
		c.JSON(http.StatusOK, gin.H{
			"diagnosis_run": run,
			"message":       "existing diagnosis returned due to matching idempotency key",
			"is_duplicate":  true,
		})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"diagnosis_run": run,
		"message":       "diagnosis accepted and queued for execution",
		"is_duplicate":  false,
	})
}

func (h *Handler) Get(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	id := c.Param("id")

	run, err := h.svc.Get(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "diagnosis run not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"diagnosis_run": run})
}

func (h *Handler) List(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	runs, total, err := h.svc.List(c.Request.Context(), userID, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"diagnosis_runs": runs,
		"total":          total,
		"page":           page,
		"page_size":      pageSize,
	})
}

func (h *Handler) Cancel(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	id := c.Param("id")

	if err := h.svc.Cancel(c.Request.Context(), id, userID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "cancellation requested",
		"id":      id,
	})
}

func (h *Handler) ListAttempts(c *gin.Context) {
	id := c.Param("id")
	attempts, err := h.svc.ListAttempts(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"attempts": attempts})
}

func (h *Handler) GetReport(c *gin.Context) {
	id := c.Param("id")
	report, err := h.reportStore.GetByRunID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "report not found"})
		return
	}

	citations, _ := h.citationStore.ListByReportID(c.Request.Context(), report.ID)

	c.JSON(http.StatusOK, gin.H{
		"report":    report,
		"citations": citations,
	})
}

func (h *Handler) GetSteps(c *gin.Context) {
	attemptID := c.Query("attempt_id")
	if attemptID == "" {
		id := c.Param("id")
		run, err := h.svc.store.GetByID(c.Request.Context(), id)
		if err != nil || run.FinalAttemptID == "" {
			c.JSON(http.StatusOK, gin.H{"steps": []trace.AgentStep{}})
			return
		}
		attemptID = run.FinalAttemptID
	}

	steps, err := h.traceStore.ListByAttempt(c.Request.Context(), attemptID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"steps": steps})
}
