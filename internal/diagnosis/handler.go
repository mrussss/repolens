package diagnosis

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/evidence"
	"repolens/internal/platform/logger"
	"repolens/internal/revision"
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
	RepositoryID       string `json:"repository_id"`
	AnalysisRevisionID string `json:"analysis_revision_id"`
	SnapshotID         string `json:"snapshot_id"`
	IssueTitle         string `json:"issue_title" binding:"required"`
	IssueDescription   string `json:"issue_description"`
	ErrorLog           string `json:"error_log"`
	IdempotencyKey     string `json:"idempotency_key"`
	CodeIndexBuildID   int64  `json:"code_index_build_id"`
	RetrievalBuildID   int64  `json:"retrieval_build_id"`
}

func (h *Handler) Create(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	var req CreateDiagnosisRequest
	if err := decodeCreateDiagnosisRequest(c, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INPUT_INVALID", "error": "invalid diagnosis request"})
		return
	}
	if req.AnalysisRevisionID == "" && (req.CodeIndexBuildID <= 0 || req.RetrievalBuildID <= 0) {
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
		UserID:             userID,
		RepositoryID:       req.RepositoryID,
		AnalysisRevisionID: req.AnalysisRevisionID,
		SnapshotID:         req.SnapshotID,
		IssueTitle:         req.IssueTitle,
		IssueDescription:   req.IssueDescription,
		ErrorLog:           req.ErrorLog,
		IdempotencyKey:     idempKey,
		CodeIndexBuildID:   req.CodeIndexBuildID,
		RetrievalBuildID:   req.RetrievalBuildID,
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
		case errors.Is(err, ErrInputTooLarge):
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"code": "PAYLOAD_TOO_LARGE", "error": "diagnosis input exceeds the configured limit"})
			return
		case errors.Is(err, revision.ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": "REVISION_NOT_FOUND", "error": "analysis revision not found"})
			return
		case errors.Is(err, ErrRevisionNotReady):
			c.JSON(http.StatusConflict, gin.H{"code": "REVISION_NOT_READY", "error": "analysis revision is not READY"})
			return
		case errors.Is(err, ErrBuildNotReady), errors.Is(err, codeintelstore.ErrBuildLineageMismatch):
			c.JSON(http.StatusConflict, gin.H{
				"code":  "BUILD_NOT_READY_OR_MISMATCHED",
				"error": "selected code index and retrieval builds are not a ready lineage",
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"code": "INPUT_INVALID", "error": "invalid diagnosis request"})
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

func decodeCreateDiagnosisRequest(c *gin.Context, request *CreateDiagnosisRequest) error {
	const maxBodyBytes = 512 * 1024
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxBodyBytes+1))
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > maxBodyBytes {
		return errors.New("invalid request body")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(request); err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("request body must contain one JSON object")
	}
	return nil
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
		if errors.Is(err, ErrRunNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "DIAGNOSIS_NOT_FOUND", "error": "diagnosis run not found"})
			return
		}
		logger.L(c.Request.Context()).Error("failed to cancel diagnosis", "diagnosis_id", id, "error", err)
		c.JSON(http.StatusConflict, gin.H{"code": "DIAGNOSIS_CANCEL_NOT_ALLOWED", "error": "diagnosis cannot be cancelled in its current state"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "cancellation requested",
		"id":      id,
	})
}

func (h *Handler) Retry(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	if err := h.svc.Retry(c.Request.Context(), c.Param("id"), userID); err != nil {
		if errors.Is(err, ErrRunNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "DIAGNOSIS_NOT_FOUND", "error": "diagnosis run not found"})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"code": "DIAGNOSIS_RETRY_NOT_ALLOWED", "error": "diagnosis retry is only available after an external provider failure"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"message": "diagnosis retry queued", "id": c.Param("id")})
}

func (h *Handler) ListAttempts(c *gin.Context) {
	id := c.Param("id")
	attempts, err := h.svc.ListAttempts(c.Request.Context(), id, c.GetString(string(logger.UserIDKey)))
	if err != nil {
		if errors.Is(err, ErrRunNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "DIAGNOSIS_NOT_FOUND", "error": "diagnosis run not found"})
			return
		}
		logger.L(c.Request.Context()).Error("failed to list diagnosis attempts", "diagnosis_id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_ERROR", "error": "internal server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"attempts": attempts})
}

func (h *Handler) GetReport(c *gin.Context) {
	id := c.Param("id")
	if _, err := h.svc.Get(c.Request.Context(), id, c.GetString(string(logger.UserIDKey))); err != nil {
		if errors.Is(err, ErrRunNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "DIAGNOSIS_NOT_FOUND", "error": "diagnosis run not found"})
			return
		}
		logger.L(c.Request.Context()).Error("failed to authorize diagnosis report", "diagnosis_id", id, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_ERROR", "error": "internal server error"})
		return
	}
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
	id := c.Param("id")
	run, runErr := h.svc.Get(c.Request.Context(), id, c.GetString(string(logger.UserIDKey)))
	if errors.Is(runErr, ErrRunNotFound) {
		c.JSON(http.StatusNotFound, gin.H{"code": "DIAGNOSIS_NOT_FOUND", "error": "diagnosis run not found"})
		return
	}
	if runErr != nil {
		logger.L(c.Request.Context()).Error("failed to authorize diagnosis steps", "diagnosis_id", id, "error", runErr)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_ERROR", "error": "internal server error"})
		return
	}
	attemptID := c.Query("attempt_id")
	if attemptID == "" {
		if run.FinalAttemptID == "" {
			c.JSON(http.StatusOK, gin.H{"steps": []trace.AgentStep{}})
			return
		}
		attemptID = run.FinalAttemptID
	} else if _, err := h.svc.GetAttemptForRun(c.Request.Context(), id, c.GetString(string(logger.UserIDKey)), attemptID); err != nil {
		if errors.Is(err, ErrRunNotFound) || errors.Is(err, ErrAttemptNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "ATTEMPT_NOT_FOUND", "error": "diagnosis attempt not found"})
			return
		}
		logger.L(c.Request.Context()).Error("failed to authorize diagnosis attempt", "diagnosis_id", id, "attempt_id", attemptID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_ERROR", "error": "internal server error"})
		return
	}

	steps, err := h.traceStore.ListByAttempt(c.Request.Context(), attemptID)
	if err != nil {
		logger.L(c.Request.Context()).Error("failed to list diagnosis steps", "diagnosis_id", id, "attempt_id", attemptID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_ERROR", "error": "internal server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"steps": steps})
}
