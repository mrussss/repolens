package codeintel

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"repolens/internal/codeintel/model"
	"repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

// CodeIndexJobHandler processes BUILD_CODE_INDEX jobs.
type CodeIndexJobHandler struct {
	store         store.Store
	snapStore     snapshot.Store
	storeFS       snapshotstore.SnapshotStore
	analyzer      *Analyzer
	revisionStore interface {
		MarkCodeIndexReady(context.Context, string, int64) error
		MarkFailed(context.Context, string, revision.Stage, string, string) error
	}
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

func (h *CodeIndexJobHandler) WithRevisionStore(store interface {
	MarkCodeIndexReady(context.Context, string, int64) error
	MarkFailed(context.Context, string, revision.Stage, string, string) error
}) *CodeIndexJobHandler {
	h.revisionStore = store
	return h
}

// Execute performs full code intelligence extraction for a code_index_build.
func (h *CodeIndexJobHandler) Execute(ctx context.Context, job *jobs.AnalysisJob) (executeErr error) {
	log := logger.L(ctx)
	buildID, err := strconv.ParseInt(job.ResourceID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid build resource ID %s: %w", job.ResourceID, err)
	}
	defer func() {
		if executeErr != nil && h.revisionStore != nil && job.AttemptCount >= job.MaxAttempts {
			if build, buildErr := h.store.GetByID(context.Background(), buildID); buildErr == nil && build.AnalysisRevisionID != "" {
				_ = h.revisionStore.MarkFailed(context.Background(), build.AnalysisRevisionID, revision.StageBuildingCode, "CODE_INDEX_BUILD_FAILED", executeErr.Error())
			}
		}
	}()

	cib, err := h.store.GetByID(ctx, buildID)
	if err != nil {
		return fmt.Errorf("failed fetching code index build %d: %w", buildID, err)
	}
	if cib.Status == model.BuildStatusReady {
		if h.revisionStore != nil && cib.AnalysisRevisionID != "" {
			if err := h.revisionStore.MarkCodeIndexReady(ctx, cib.AnalysisRevisionID, cib.ID); err != nil && !errors.Is(err, revision.ErrLineage) {
				return jobs.NewRetryableError("REVISION_STAGE_UPDATE_FAILED", err.Error(), err)
			}
		}
		return nil
	}
	if err := h.store.MarkBuildBuilding(ctx, cib.ID); err != nil {
		return jobs.NewRetryableError("BUILD_STATE_UPDATE_FAILED", err.Error(), err)
	}

	snap, err := h.snapStore.GetByID(ctx, cib.SnapshotID)
	if err != nil {
		return fmt.Errorf("failed fetching snapshot %s for build: %w", cib.SnapshotID, err)
	}
	if snap.Status != snapshot.StatusReady {
		return jobs.NewRetryableError("SNAPSHOT_NOT_READY", "snapshot is not READY", nil)
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
		// Terminal business transitions are claim-fenced by the Job Store.
		return err
	}

	var saveErr error
	if finalizer, ok := h.store.(interface {
		FinalizeCodeIndexSuccess(context.Context, int64, string, string, int64, *model.AnalysisResult) error
	}); ok && job.WorkerID != nil && job.ClaimToken != nil {
		saveErr = finalizer.FinalizeCodeIndexSuccess(ctx, job.ID, *job.WorkerID, *job.ClaimToken, cib.ID, analysisRes)
	} else {
		saveErr = h.store.SaveAnalysisResult(ctx, cib.ID, analysisRes)
	}
	if saveErr != nil {
		log.Error("failed persisting code index analysis result", "build_id", cib.ID, "error", saveErr)
		return saveErr
	}

	// Auto-create/trigger BUILD_RETRIEVAL job for derived retrieval index
	_, _, _ = h.store.GetOrCreateRetrievalBuild(ctx, cib.ID, "BM25")
	if h.revisionStore != nil && cib.AnalysisRevisionID != "" {
		if err := h.revisionStore.MarkCodeIndexReady(ctx, cib.AnalysisRevisionID, cib.ID); err != nil {
			return jobs.NewRetryableError("REVISION_STAGE_UPDATE_FAILED", err.Error(), err)
		}
	}

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
