package codeintel

import (
	"context"
	"fmt"
	"strconv"

	"repolens/internal/codeintel/model"
	"repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/snapshot"
)

// CodeIndexJobHandler processes BUILD_CODE_INDEX jobs.
type CodeIndexJobHandler struct {
	store     store.Store
	snapStore snapshot.Store
	storeFS   snapshotstore.SnapshotStore
	analyzer  *Analyzer
}

// NewCodeIndexJobHandler constructs a new handler for BUILD_CODE_INDEX jobs.
func NewCodeIndexJobHandler(
	store store.Store,
	snapStore snapshot.Store,
	storeFS snapshotstore.SnapshotStore,
	analyzer *Analyzer,
) *CodeIndexJobHandler {
	if analyzer == nil {
		analyzer = NewAnalyzer()
	}
	return &CodeIndexJobHandler{
		store:     store,
		snapStore: snapStore,
		storeFS:   storeFS,
		analyzer:  analyzer,
	}
}

// Execute performs full code intelligence extraction for a code_index_build.
func (h *CodeIndexJobHandler) Execute(ctx context.Context, job *jobs.AnalysisJob) error {
	log := logger.L(ctx)
	buildID, err := strconv.ParseInt(job.ResourceID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid build resource ID %s: %w", job.ResourceID, err)
	}

	cib, err := h.store.GetByID(ctx, buildID)
	if err != nil {
		return fmt.Errorf("failed fetching code index build %d: %w", buildID, err)
	}
	if cib.Status == model.BuildStatusReady {
		return nil
	}
	if err := h.store.MarkBuildBuilding(ctx, cib.ID); err != nil {
		return jobs.NewRetryableError("BUILD_STATE_UPDATE_FAILED", err.Error(), err)
	}

	snap, err := h.snapStore.GetByID(ctx, cib.SnapshotID)
	if err != nil {
		return fmt.Errorf("failed fetching snapshot %s for build: %w", cib.SnapshotID, err)
	}

	snapshotDir := h.storeFS.GetSourcePath(snap.RepositoryID, snap.ID)
	log.Info("starting code index build execution", "build_id", cib.ID, "snapshot_id", snap.ID, "path", snapshotDir)

	bc := model.BuildContext{
		GOOS:   cib.GOOS,
		GOARCH: cib.GOARCH,
	}

	analysisRes, err := h.analyzer.Analyze(ctx, snapshotDir, bc)
	if err != nil {
		log.Error("code index build analysis failed", "build_id", cib.ID, "error", err)
		class, _ := jobs.ClassifyError(err)
		if class == jobs.ErrorClassPermanent || job.AttemptCount >= job.MaxAttempts {
			_ = h.store.FailBuild(ctx, cib.ID, err.Error())
		}
		return err
	}

	if err := h.store.SaveAnalysisResult(ctx, cib.ID, analysisRes); err != nil {
		log.Error("failed persisting code index analysis result", "build_id", cib.ID, "error", err)
		if job.AttemptCount >= job.MaxAttempts {
			_ = h.store.FailBuild(ctx, cib.ID, err.Error())
		}
		return err
	}

	// Auto-create/trigger BUILD_RETRIEVAL job for derived retrieval index
	_, _, _ = h.store.GetOrCreateRetrievalBuild(ctx, cib.ID, "BM25")

	log.Info("code index build completed successfully",
		"build_id", cib.ID,
		"symbols", len(analysisRes.Symbols),
		"relations", len(analysisRes.Relations),
		"quality_parsed_pct", fmt.Sprintf("%.1f%%", float64(analysisRes.Quality.FilesParsed)/float64(max(1, analysisRes.Quality.FilesTotal))*100),
	)

	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
