package revision

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"repolens/internal/platform/logger"
)

type Handler struct{ service *Service }

func NewHandler(service *Service) *Handler { return &Handler{service: service} }

type prepareRequest struct {
	Ref string `json:"ref"`
}

func decodePrepareRequest(c *gin.Context) (prepareRequest, error) {
	var request prepareRequest
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1024*1024+1))
	if err != nil {
		return request, err
	}
	if len(body) == 0 {
		return request, errors.New("request body is required; use {} when the repository default ref is desired")
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		return request, errors.New("request body must contain one JSON object")
	}
	return request, nil
}

func (h *Handler) Create(c *gin.Context) {
	request, err := decodePrepareRequest(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INPUT_INVALID", "error": "invalid revision request"})
		return
	}
	userID := c.GetString(string(logger.UserIDKey))
	value, created, err := h.service.Prepare(c.Request.Context(), userID, c.Param("id"), request.Ref)
	if err != nil {
		switch {
		case errors.Is(err, ErrFailedRevision):
			c.JSON(http.StatusConflict, gin.H{"code": "REVISION_PREPARE_FAILED", "error": "analysis revision failed; retry explicitly", "analysis_revision": publicRevision(value)})
		case errors.Is(err, ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": "REPOSITORY_NOT_FOUND", "error": "repository not found"})
		case errors.Is(err, ErrRefResolution):
			c.JSON(http.StatusBadRequest, gin.H{"code": "REF_NOT_FOUND", "error": "repository ref could not be resolved"})
		default:
			logger.L(c.Request.Context()).Error("failed to prepare analysis revision", "repository_id", c.Param("id"), "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_ERROR", "error": "internal server error"})
		}
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	c.JSON(status, gin.H{"analysis_revision": publicRevision(value), "message": "analysis revision is preparing", "created": created})
}

func (h *Handler) Get(c *gin.Context) {
	value, err := h.service.Get(c.Request.Context(), c.GetString(string(logger.UserIDKey)), c.Param("id"))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "REVISION_NOT_FOUND", "error": "analysis revision not found"})
			return
		}
		logger.L(c.Request.Context()).Error("failed to get analysis revision", "revision_id", c.Param("id"), "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_ERROR", "error": "internal server error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"analysis_revision": publicRevision(value)})
}

func (h *Handler) List(c *gin.Context) {
	limit, err := strconv.Atoi(c.DefaultQuery("limit", "20"))
	if err != nil || limit < 1 || limit > 100 {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INPUT_INVALID", "error": "limit must be between 1 and 100"})
		return
	}
	values, err := h.service.List(c.Request.Context(), c.GetString(string(logger.UserIDKey)), c.Param("id"), limit)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"code": "REPOSITORY_NOT_FOUND", "error": "repository not found"})
			return
		}
		logger.L(c.Request.Context()).Error("failed to list analysis revisions", "repository_id", c.Param("id"), "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_ERROR", "error": "internal server error"})
		return
	}
	publicValues := make([]AnalysisRevision, 0, len(values))
	for _, value := range values {
		publicValues = append(publicValues, *publicRevision(&value))
	}
	c.JSON(http.StatusOK, gin.H{"analysis_revisions": publicValues})
}

func (h *Handler) Retry(c *gin.Context) {
	value, err := h.service.Retry(c.Request.Context(), c.GetString(string(logger.UserIDKey)), c.Param("id"))
	if err != nil {
		switch {
		case errors.Is(err, ErrNotFound):
			c.JSON(http.StatusNotFound, gin.H{"code": "REVISION_NOT_FOUND", "error": "analysis revision not found"})
		case errors.Is(err, ErrInvalidState):
			c.JSON(http.StatusConflict, gin.H{"code": "REVISION_NOT_FAILED", "error": "only a failed revision can be retried"})
		default:
			logger.L(c.Request.Context()).Error("failed to retry analysis revision", "revision_id", c.Param("id"), "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"code": "INTERNAL_ERROR", "error": "internal server error"})
		}
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"analysis_revision": publicRevision(value), "message": "analysis revision retry queued"})
}

func publicRevision(value *AnalysisRevision) *AnalysisRevision {
	if value == nil {
		return nil
	}
	copyOfValue := *value
	if copyOfValue.ErrorMessage != "" {
		copyOfValue.ErrorMessage = "analysis revision failed during " + strings.ToLower(string(copyOfValue.Stage))
	}
	return &copyOfValue
}
