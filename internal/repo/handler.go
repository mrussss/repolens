package repo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"repolens/internal/jobs"
	"repolens/internal/platform/config"
	"repolens/internal/platform/logger"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
)

type Handler struct {
	repoSvc          *Service
	snapshotStore    snapshot.Store
	indexStore       repoindex.Store
	db               *gorm.DB
	resolver         SnapshotResolver
	jobStore         *jobs.Store
	snapshotBasePath string
}

type SnapshotResolver interface {
	ResolveRef(ctx context.Context, gitURL, ref string) (string, error)
}

func NewHandler(repoSvc *Service, snapshotStore snapshot.Store, indexStore repoindex.Store, db *gorm.DB) *Handler {
	return &Handler{
		repoSvc:       repoSvc,
		snapshotStore: snapshotStore,
		indexStore:    indexStore,
		db:            db,
	}
}

func (h *Handler) WithSnapshotResolver(resolver SnapshotResolver, jobStore *jobs.Store) *Handler {
	h.resolver = resolver
	h.jobStore = jobStore
	return h
}

func (h *Handler) WithSnapshotBasePath(basePath string) *Handler {
	h.snapshotBasePath = basePath
	return h
}

type RegisterRepoRequest struct {
	Name       string `json:"name" binding:"required"`
	GitURL     string `json:"git_url" binding:"required"`
	DefaultRef string `json:"default_ref"`
}

type TriggerIndexRequest struct {
	Ref      string                      `json:"ref"`
	Strategy repoindex.RetrievalStrategy `json:"strategy"`
}

func decodeTriggerIndexRequest(body io.Reader, req *TriggerIndexRequest) error {
	decoder := json.NewDecoder(body)
	var raw json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}

	object := bytes.TrimSpace(raw)
	if len(object) == 0 || object[0] != '{' {
		return errors.New("expected JSON object")
	}
	if !utf8.Valid(object) {
		return errors.New("invalid UTF-8")
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON documents")
		}
		return err
	}
	if err := validateTriggerIndexFieldNames(object); err != nil {
		return err
	}

	strictDecoder := json.NewDecoder(bytes.NewReader(object))
	strictDecoder.DisallowUnknownFields()
	if err := strictDecoder.Decode(req); err != nil {
		return err
	}
	return nil
}

func validateTriggerIndexFieldNames(object []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(object))
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return errors.New("expected JSON object")
	}

	seen := make(map[string]struct{}, 2)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		field, ok := token.(string)
		if !ok || (field != "ref" && field != "strategy") {
			return errors.New("unknown field")
		}
		if _, exists := seen[field]; exists {
			return errors.New("duplicate field")
		}
		seen[field] = struct{}{}

		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		return err
	}
	return nil
}

func applyTriggerIndexDefaults(req *TriggerIndexRequest, defaultRef string) {
	if req.Ref == "" {
		req.Ref = defaultRef
	}
	if req.Strategy == "" {
		req.Strategy = repoindex.StrategyBM25
	}
}

func (h *Handler) Register(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	var req RegisterRepoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INVALID_REPOSITORY_REQUEST", "error": "invalid repository request"})
		return
	}

	r, err := h.repoSvc.Register(c.Request.Context(), userID, req.Name, req.GitURL, req.DefaultRef)
	if err != nil {
		logger.L(c.Request.Context()).Error("failed to register repository", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "REPOSITORY_CREATE_FAILED", "error": "failed to create repository"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"repository": r})
}

func (h *Handler) Get(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	id := c.Param("id")

	r, err := h.repoSvc.Get(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "REPOSITORY_NOT_FOUND", "error": "repository not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"repository": r})
}

func (h *Handler) List(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	status := c.Query("status")
	repos, total, err := h.repoSvc.List(c.Request.Context(), userID, page, pageSize, status)
	if err != nil {
		logger.L(c.Request.Context()).Error("failed to list repositories", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "REPOSITORY_LIST_FAILED", "error": "failed to list repositories"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"repositories": repos,
		"total":        total,
		"page":         page,
		"page_size":    pageSize,
	})
}

func (h *Handler) TriggerIndex(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	repoID := c.Param("id")

	r, err := h.repoSvc.Get(c.Request.Context(), repoID, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "repository not found"})
		return
	}

	var req TriggerIndexRequest
	if err := decodeTriggerIndexRequest(c.Request.Body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"code": "INPUT_INVALID", "error": "invalid index request"})
		return
	}
	applyTriggerIndexDefaults(&req, r.DefaultRef)
	if h.resolver == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "snapshot resolver is not configured"})
		return
	}
	commitSHA, err := h.resolver.ResolveRef(c.Request.Context(), r.GitURL, req.Ref)
	if err != nil {
		logger.L(c.Request.Context()).Error("failed to resolve repository ref", "error", err)
		c.JSON(http.StatusBadRequest, gin.H{"code": "REF_RESOLUTION_FAILED", "error": "failed to resolve repository ref"})
		return
	}
	if existing, lookupErr := h.snapshotStore.GetByCommit(c.Request.Context(), repoID, commitSHA); lookupErr == nil {
		if existing.Status == snapshot.StatusFailed && h.jobStore != nil {
			if requeueErr := h.jobStore.ManualRequeue(c.Request.Context(), jobs.JobTypeMaterializeSnapshot, existing.ID); requeueErr != nil {
				logger.L(c.Request.Context()).Error("failed to requeue failed snapshot", "error", requeueErr)
				c.JSON(http.StatusConflict, gin.H{"code": "SNAPSHOT_REQUEUE_FAILED", "error": "snapshot already failed and cannot be requeued"})
				return
			}
			existing, _ = h.snapshotStore.GetByID(c.Request.Context(), existing.ID)
		}
		c.JSON(http.StatusAccepted, gin.H{"snapshot": existing, "message": "snapshot already exists for this commit"})
		return
	}

	snapID := uuid.New().String()
	basePath := h.snapshotBasePath
	if basePath == "" {
		basePath = config.DefaultSnapshotBasePath()
	}
	matPath := filepath.Join(basePath, repoID, snapID, "source")

	snap := &snapshot.RepositorySnapshot{
		ID:               snapID,
		RepositoryID:     repoID,
		CommitSHA:        commitSHA,
		Ref:              req.Ref,
		MaterializedPath: matPath,
		ContentHash:      "",
		Status:           snapshot.StatusMaterializing,
	}

	job := &jobs.AnalysisJob{
		JobType:             jobs.JobTypeMaterializeSnapshot,
		ResourceID:          snapID,
		Status:              jobs.StatusPending,
		ExecutionGeneration: 1,
		AttemptCount:        0,
		MaxAttempts:         3,
		NextRunAt:           time.Now().UTC(),
	}

	err = h.db.WithContext(c.Request.Context()).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(snap).Error; err != nil {
			return err
		}
		if err := tx.Create(job).Error; err != nil {
			return err
		}
		return nil
	})

	if err != nil {
		if existing, lookupErr := h.snapshotStore.GetByCommit(c.Request.Context(), repoID, commitSHA); lookupErr == nil {
			c.JSON(http.StatusAccepted, gin.H{"snapshot": existing, "message": "snapshot already exists for this commit"})
			return
		}
		logger.L(c.Request.Context()).Error("failed to trigger snapshot indexing", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"code": "SNAPSHOT_QUEUE_FAILED", "error": "failed to queue snapshot indexing"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"snapshot": snap,
		"message":  "snapshot creation and indexing queued",
	})
}
