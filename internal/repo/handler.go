package repo

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"

	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
)

type Handler struct {
	repoSvc       *Service
	snapshotStore snapshot.Store
	indexStore    repoindex.Store
	db            *gorm.DB
}

func NewHandler(repoSvc *Service, snapshotStore snapshot.Store, indexStore repoindex.Store, db *gorm.DB) *Handler {
	return &Handler{
		repoSvc:       repoSvc,
		snapshotStore: snapshotStore,
		indexStore:    indexStore,
		db:            db,
	}
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

func (h *Handler) Register(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	var req RegisterRepoRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	r, err := h.repoSvc.Register(c.Request.Context(), userID, req.Name, req.GitURL, req.DefaultRef)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"repository": r})
}

func (h *Handler) Get(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	id := c.Param("id")

	r, err := h.repoSvc.Get(c.Request.Context(), id, userID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"repository": r})
}

func (h *Handler) List(c *gin.Context) {
	userID := c.GetString(string(logger.UserIDKey))
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))

	repos, total, err := h.repoSvc.List(c.Request.Context(), userID, page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
	if err := c.ShouldBindJSON(&req); err != nil {
		req.Ref = r.DefaultRef
		req.Strategy = repoindex.StrategyBM25
	}
	if req.Ref == "" {
		req.Ref = r.DefaultRef
	}
	if req.Strategy == "" {
		req.Strategy = repoindex.StrategyBM25
	}

	snapID := uuid.New().String()
	matPath := "/data/repositories/" + repoID + "/" + snapID + "/source"

	snap := &snapshot.RepositorySnapshot{
		ID:               snapID,
		RepositoryID:     repoID,
		CommitSHA:        "pending", // will be updated upon git clone checkout
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to trigger indexing: " + err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"snapshot": snap,
		"message":  "snapshot creation and indexing queued",
	})
}
