package diagnosis

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	codeintelstore "repolens/internal/codeintel/store"
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
	CodeIndexBuildID int64  `json:"code_index_build_id"`
	RetrievalBuildID int64  `json:"retrieval_build_id"`
}

func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	var req CreateDiagnosisRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.CodeIndexBuildID <= 0 || req.RetrievalBuildID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":  "INVALID_BUILD_SELECTION",
			"error": "code_index_build_id and retrieval_build_id must be positive",
		})
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
		CodeIndexBuildID: req.CodeIndexBuildID,
		RetrievalBuildID: req.RetrievalBuildID,
	}

	run, created, err := h.svc.Create(c.Request.Context(), input)
	if err != nil {
		switch {
		case errors.Is(err, ErrIdempotencyConflict):
			c.JSON(http.StatusConflict, gin.H{"error": "Idempotency conflict: key reused with differing request payload"})
			return
		case errors.Is(err, ErrProviderNotConfigured):
			c.JSON(http.StatusFailedDependency, gin.H{
				"code":  "PROVIDER_NOT_CONFIGURED",
				"error": "provider is not configured",
			})
			return
		case errors.Is(err, ErrInvalidBuildSelection):
			c.JSON(http.StatusBadRequest, gin.H{
				"code":  "INVALID_BUILD_SELECTION",
				"error": "code_index_build_id and retrieval_build_id must be positive",
			})
			return
		case errors.Is(err, ErrBuildNotReady), errors.Is(err, codeintelstore.ErrBuildLineageMismatch):
			c.JSON(http.StatusConflict, gin.H{
				"code":  "BUILD_NOT_READY_OR_MISMATCHED",
				"error": "selected code index and retrieval builds are not a ready lineage",
			})
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
		if errors.Is(err, ErrRunNotFound) {
			c.JSON(http.StatusNotFound, gin.H{
				"code":  "DIAGNOSIS_NOT_FOUND",
				"error": "diagnosis run not found",
			})
			return
		}
		logger.L(c.Request.Context()).Error("failed to get diagnosis run", "diagnosis_id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":  "INTERNAL_ERROR",
			"error": "internal server error",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{"diagnosis_run": run})
}

func (h *Handler) List(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	page, pageSize, err := parsePagination(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"code":  "INVALID_PAGINATION",
			"error": err.Error(),
		})
		return
	}

	runs, total, err := h.svc.List(c.Request.Context(), userID, page, pageSize)
	if err != nil {
		logger.L(c.Request.Context()).Error("failed to list diagnosis runs", "page", page, "page_size", pageSize, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{
			"code":  "INTERNAL_ERROR",
			"error": "internal server error",
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"diagnosis_runs": runs,
		"total":          total,
		"page":           page,
		"page_size":      pageSize,
	})
}

func parsePagination(c *gin.Context) (page, pageSize int, err error) {
	page, err = strconv.Atoi(c.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		return 0, 0, fmt.Errorf("page must be a positive integer")
	}

	pageSize, err = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if err != nil || pageSize < 1 || pageSize > 100 {
		return 0, 0, fmt.Errorf("page_size must be an integer between 1 and 100")
	}

	return page, pageSize, nil
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
