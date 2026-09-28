package codeintel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"repolens/internal/codeintel/model"
	"repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/snapshot"
)

var (
	errRevisionRetryRequired = errors.New("revision-aware builds must be retried through the revision API")
	errManualRequeueNotReady = errors.New("build job is not FAILED with RETRYABLE_EXHAUSTED")
)

type requeueStore interface {
	GetJobByResource(context.Context, jobs.JobType, string) (*jobs.AnalysisJob, error)
	ManualRequeue(context.Context, jobs.JobType, string) error
}

// Handler serves Code Intelligence REST APIs.
type Handler struct {
	ciStore       store.Store
	snapshotStore snapshot.Store
	jobStore      requeueStore
}

func (h *Handler) WithJobStore(jobStore requeueStore) *Handler {
	h.jobStore = jobStore
	return h
}

// NewHandler constructs a new Code Intelligence handler.
func NewHandler(ciStore store.Store, snapshotStore snapshot.Store) *Handler {
	return &Handler{
		ciStore:       ciStore,
		snapshotStore: snapshotStore,
	}
}

// TriggerCodeIndexBuild handles POST /api/v1/snapshots/:id/code-index-builds
func (h *Handler) TriggerCodeIndexBuild(c *gin.Context) {
	snapID := c.Param("id")
	ctx := c.Request.Context()

	snap, err := h.snapshotStore.GetByID(ctx, snapID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "snapshot not found"})
		return
	}
	if snap.Status != snapshot.StatusReady {
		c.JSON(http.StatusConflict, gin.H{"error": "snapshot is not in READY status", "status": snap.Status})
		return
	}

	bc := model.DefaultBuildContext()
	build, created, err := h.ciStore.GetOrCreateBuild(ctx, snap.ID, snap.RepositoryID, bc)
	if err != nil {
		writeCodeIntelInternalError(c, "CODE_INDEX_BUILD_CREATE_FAILED", "failed to create code index build", err)
		return
	}
	requeued := false
	if !created && build.Status == model.BuildStatusFailed {
		requeued, err = h.requeueFailedBuild(ctx, jobs.JobTypeBuildCodeIndex, strconv.FormatInt(build.ID, 10), build.AnalysisRevisionID)
		if err != nil {
			if errors.Is(err, errRevisionRetryRequired) || errors.Is(err, errManualRequeueNotReady) {
				c.JSON(http.StatusConflict, gin.H{"code": "CODE_INDEX_BUILD_REQUEUE_NOT_ALLOWED", "error": err.Error()})
				return
			}
			writeCodeIntelInternalError(c, "CODE_INDEX_BUILD_REQUEUE_FAILED", "failed to requeue code index build", err)
			return
		}
		build, err = h.ciStore.GetByID(ctx, build.ID)
		if err != nil {
			writeCodeIntelInternalError(c, "CODE_INDEX_BUILD_RELOAD_FAILED", "failed to reload code index build", err)
			return
		}
	}

	if created || requeued {
		c.JSON(http.StatusAccepted, gin.H{"code_index_build": build, "status": "CREATED"})
	} else {
		c.JSON(http.StatusOK, gin.H{"code_index_build": build, "status": build.Status})
	}
}

// GetCodeIndexBuild handles GET /api/v1/code-index-builds/:id
func (h *Handler) GetCodeIndexBuild(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid build id"})
		return
	}

	build, err := h.ciStore.GetByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "CODE_INDEX_BUILD_NOT_FOUND", "error": "code index build not found"})
		return
	}

	c.JSON(http.StatusOK, build)
}

// GetQuality handles GET /api/v1/code-index-builds/:id/quality
func (h *Handler) GetQuality(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid build id"})
		return
	}

	build, err := h.ciStore.GetByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "CODE_INDEX_BUILD_NOT_FOUND", "error": "code index build not found"})
		return
	}

	quality := gin.H{
		"code_index_build_id":       build.ID,
		"snapshot_id":               build.SnapshotID,
		"files_total":               build.FilesTotal,
		"files_parsed":              build.FilesParsed,
		"files_failed":              build.FilesFailed,
		"parsed_pct":                calcPct(build.FilesParsed, build.FilesTotal),
		"packages_total":            build.PackagesTotal,
		"packages_typechecked":      build.PackagesTypechecked,
		"packages_failed":           build.PackagesFailed,
		"typechecked_pct":           calcPct(build.PackagesTypechecked, build.PackagesTotal),
		"symbol_count":              build.SymbolCount,
		"semantic_relation_count":   build.SemanticRelationCount,
		"syntactic_relation_count":  build.SyntacticRelationCount,
		"heuristic_relation_count":  build.HeuristicRelationCount,
		"unresolved_relation_count": build.UnresolvedRelationCount,
		"symlinks_skipped":          build.SymlinksSkipped,
		"status":                    build.Status,
	}
	var warnings []string
	if build.QualityWarningsJSON != "" && json.Unmarshal([]byte(build.QualityWarningsJSON), &warnings) == nil {
		quality["warnings"] = warnings
	}

	c.JSON(http.StatusOK, quality)
}

// ListSymbols handles GET /api/v1/code-index-builds/:id/symbols
func (h *Handler) ListSymbols(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid build id"})
		return
	}

	query := c.Query("q")
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "50"))

	symbols, err := h.ciStore.ListSymbols(c.Request.Context(), id, query, limit)
	if err != nil {
		writeCodeIntelInternalError(c, "CODE_SYMBOL_LIST_FAILED", "failed to list code symbols", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"symbols": symbols, "total": len(symbols)})
}

// GetSymbol handles GET /api/v1/symbols/:id
func (h *Handler) GetSymbol(c *gin.Context) {
	idStr := c.Param("id")
	_, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid symbol id"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "symbol details", "id": idStr})
}

// GetSymbolReferences handles GET /api/v1/symbols/:id/references
func (h *Handler) GetSymbolReferences(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid symbol id"})
		return
	}

	cibIDStr := c.Query("code_index_build_id")
	cibID, _ := strconv.ParseInt(cibIDStr, 10, 64)

	rels, err := h.ciStore.ListRelationsForSymbol(c.Request.Context(), cibID, id)
	if err != nil {
		writeCodeIntelInternalError(c, "CODE_REFERENCE_LIST_FAILED", "failed to list symbol references", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"relations": rels, "total": len(rels)})
}

// GetSymbolTests handles GET /api/v1/symbols/:id/tests
func (h *Handler) GetSymbolTests(c *gin.Context) {
	keyHash := c.Query("symbol_key_hash")
	cibIDStr := c.Query("code_index_build_id")
	cibID, _ := strconv.ParseInt(cibIDStr, 10, 64)

	tests, err := h.ciStore.ListRelatedTests(c.Request.Context(), cibID, keyHash)
	if err != nil {
		writeCodeIntelInternalError(c, "CODE_TEST_LIST_FAILED", "failed to list related tests", err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"related_tests": tests, "total": len(tests)})
}

// TriggerRetrievalBuild handles POST /api/v1/code-index-builds/:id/retrieval-builds
func (h *Handler) TriggerRetrievalBuild(c *gin.Context) {
	idStr := c.Param("id")
	cibID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid code index build id"})
		return
	}

	cib, err := h.ciStore.GetByID(c.Request.Context(), cibID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "code index build not found"})
		return
	}
	if cib.Status != model.BuildStatusReady {
		c.JSON(http.StatusConflict, gin.H{"error": "code index build is not READY", "status": cib.Status})
		return
	}

	strategy := c.DefaultQuery("strategy", "BM25")
	rb, created, err := h.ciStore.GetOrCreateRetrievalBuild(c.Request.Context(), cib.ID, strategy)
	if err != nil {
		writeCodeIntelInternalError(c, "RETRIEVAL_BUILD_CREATE_FAILED", "failed to create retrieval build", err)
		return
	}
	requeued := false
	if !created && rb.Status == model.BuildStatusFailed {
		requeued, err = h.requeueFailedBuild(c.Request.Context(), jobs.JobTypeBuildRetrieval, strconv.FormatInt(rb.ID, 10), rb.AnalysisRevisionID)
		if err != nil {
			if errors.Is(err, errRevisionRetryRequired) || errors.Is(err, errManualRequeueNotReady) {
				c.JSON(http.StatusConflict, gin.H{"code": "RETRIEVAL_BUILD_REQUEUE_NOT_ALLOWED", "error": err.Error()})
				return
			}
			writeCodeIntelInternalError(c, "RETRIEVAL_BUILD_REQUEUE_FAILED", "failed to requeue retrieval build", err)
			return
		}
		rb, err = h.ciStore.GetRetrievalBuildByID(c.Request.Context(), rb.ID)
		if err != nil {
			writeCodeIntelInternalError(c, "RETRIEVAL_BUILD_RELOAD_FAILED", "failed to reload retrieval build", err)
			return
		}
	}

	if created || requeued {
		c.JSON(http.StatusAccepted, gin.H{"retrieval_build": rb, "status": "CREATED"})
	} else {
		c.JSON(http.StatusOK, gin.H{"retrieval_build": rb, "status": rb.Status})
	}
}

func (h *Handler) requeueFailedBuild(ctx context.Context, jobType jobs.JobType, resourceID, analysisRevisionID string) (bool, error) {
	if analysisRevisionID != "" {
		return false, errRevisionRetryRequired
	}
	if h.jobStore == nil {
		return false, errors.New("job store is not configured")
	}
	job, err := h.jobStore.GetJobByResource(ctx, jobType, resourceID)
	if err != nil {
		return false, fmt.Errorf("load build job: %w", err)
	}
	if job.Status != jobs.StatusFailed || job.TerminalReason == nil || *job.TerminalReason != jobs.TerminalReasonRetryableExhausted {
		return false, errManualRequeueNotReady
	}
	if err := h.jobStore.ManualRequeue(ctx, jobType, resourceID); err != nil {
		// A concurrent request may already have performed the same requeue.
		latest, lookupErr := h.jobStore.GetJobByResource(ctx, jobType, resourceID)
		if lookupErr == nil && latest.Status != jobs.StatusFailed {
			return false, nil
		}
		return false, fmt.Errorf("manual requeue failed: %w", err)
	}
	return true, nil
}

// GetRetrievalBuild handles GET /api/v1/retrieval-builds/:id
func (h *Handler) GetRetrievalBuild(c *gin.Context) {
	idStr := c.Param("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid retrieval build id"})
		return
	}

	rb, err := h.ciStore.GetRetrievalBuildByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"code": "RETRIEVAL_BUILD_NOT_FOUND", "error": "retrieval build not found"})
		return
	}

	c.JSON(http.StatusOK, rb)
}

func calcPct(num, den int) string {
	if den <= 0 {
		return "0.0%"
	}
	return fmt.Sprintf("%.1f%%", float64(num)/float64(den)*100.0)
}

func writeCodeIntelInternalError(c *gin.Context, code, message string, err error) {
	logger.L(c.Request.Context()).Error(message, "error", err)
	c.JSON(http.StatusInternalServerError, gin.H{"code": code, "error": message})
}
