package codeintel

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"

	"repolens/internal/codeintel/model"
	"repolens/internal/codeintel/store"
	"repolens/internal/snapshot"
)

// Handler serves Code Intelligence REST APIs.
type Handler struct {
	ciStore       store.Store
	snapshotStore snapshot.Store
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if created {
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
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	quality := gin.H{
		"code_index_build_id":        build.ID,
		"snapshot_id":                build.SnapshotID,
		"files_total":                build.FilesTotal,
		"files_parsed":               build.FilesParsed,
		"files_failed":               build.FilesFailed,
		"parsed_pct":                 calcPct(build.FilesParsed, build.FilesTotal),
		"packages_total":             build.PackagesTotal,
		"packages_typechecked":       build.PackagesTypechecked,
		"packages_failed":            build.PackagesFailed,
		"typechecked_pct":            calcPct(build.PackagesTypechecked, build.PackagesTotal),
		"symbol_count":               build.SymbolCount,
		"semantic_relation_count":    build.SemanticRelationCount,
		"syntactic_relation_count":   build.SyntacticRelationCount,
		"heuristic_relation_count":   build.HeuristicRelationCount,
		"unresolved_relation_count":  build.UnresolvedRelationCount,
		"status":                     build.Status,
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if created {
		c.JSON(http.StatusAccepted, gin.H{"retrieval_build": rb, "status": "CREATED"})
	} else {
		c.JSON(http.StatusOK, gin.H{"retrieval_build": rb, "status": rb.Status})
	}
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
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
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
