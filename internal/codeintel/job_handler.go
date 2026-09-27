package codeintel

import (
	"context"
	"encoding/json"
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
	"repolens/internal/snapshotpolicy"
)

// CodeIndexJobHandler processes BUILD_CODE_INDEX jobs.
type CodeIndexJobHandler struct {
	store         store.Store
	snapStore     snapshot.Store
	storeFS       snapshotstore.SnapshotStore
	analyzer      *Analyzer
	revisionStore interface {
		MarkCodeIndexReady(context.Context, string, int64) error
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
}) *CodeIndexJobHandler {
	h.revisionStore = store
	return h
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
		if h.revisionStore != nil && cib.AnalysisRevisionID != "" {
			if err := h.revisionStore.MarkCodeIndexReady(ctx, cib.AnalysisRevisionID, cib.ID); err != nil && !errors.Is(err, revision.ErrLineage) {
				return jobs.NewRetryableError("REVISION_STAGE_UPDATE_FAILED", err.Error(), err)
			}
		}
		return nil
	}

	var buildTags []string
	if cib.BuildTagsJSON != "" {
		if err := json.Unmarshal([]byte(cib.BuildTagsJSON), &buildTags); err != nil {
			return jobs.NewPermanentError("BUILD_TAGS_UNAVAILABLE", fmt.Sprintf("invalid persisted build tags for build %d", cib.ID), err)
		}
	}
	bc := model.BuildContext{GOOS: cib.GOOS, GOARCH: cib.GOARCH, BuildTags: buildTags}
	if bc.BuildTagsHash() != cib.BuildTagsHash {
		return jobs.NewPermanentError("BUILD_TAGS_UNAVAILABLE", fmt.Sprintf("persisted build tags do not match build %d identity; legacy tag names may be unavailable", cib.ID), nil)
	}
	if bc.BuildContextHash() != cib.BuildContextHash {
		return fmt.Errorf("persisted build context does not match build %d identity", cib.ID)
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

	var allowedFiles []string
	manifest, manifestErr := snapshotpolicy.LoadManifest(snapshotDir)
	if manifestErr == nil {
		if manifest.SnapshotID != snap.ID || manifest.CommitSHA != snap.CommitSHA || manifest.ContentHash != snap.ContentHash {
			return jobs.NewPermanentError("SNAPSHOT_MANIFEST_IDENTITY_MISMATCH", "snapshot manifest does not match READY snapshot identity", nil)
		}
		allowedFiles = manifest.AllowedPaths()
	} else if errors.Is(manifestErr, snapshotpolicy.ErrManifestNotFound) && snapshotpolicy.RequiresManifest(snapshotDir) {
		return jobs.NewPermanentError("SNAPSHOT_MANIFEST_MISSING", "immutable READY snapshot is missing its file manifest", manifestErr)
	} else if !errors.Is(manifestErr, snapshotpolicy.ErrManifestNotFound) {
		return jobs.NewRetryableError("SNAPSHOT_MANIFEST_UNAVAILABLE", manifestErr.Error(), manifestErr)
	}
	analysisRes, err := h.analyzer.AnalyzeWithAllowedFiles(ctx, snapshotDir, allowedFiles, bc)
	if err != nil {
		log.Error("code index build analysis failed", "build_id", cib.ID, "error", err)
		// Terminal business transitions are claim-fenced by the Job Store.
		return err
	}

	var saveErr error
	stageFinalized := false
	if finalizer, ok := h.store.(interface {
		FinalizeCodeIndexSuccessWithRevision(context.Context, int64, string, string, int64, string, *model.AnalysisResult) error
	}); ok && job.WorkerID != nil && job.ClaimToken != nil && cib.AnalysisRevisionID != "" {
		saveErr = finalizer.FinalizeCodeIndexSuccessWithRevision(ctx, job.ID, *job.WorkerID, *job.ClaimToken, cib.ID, cib.AnalysisRevisionID, analysisRes)
		stageFinalized = saveErr == nil
	} else if finalizer, ok := h.store.(interface {
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

	// The revision-aware finalizer creates the next stage in the same
	// transaction. Keep this fallback for legacy/non-revision stores only.
	if !stageFinalized {
		_, _, _ = h.store.GetOrCreateRetrievalBuild(ctx, cib.ID, "BM25")
	}
	if !stageFinalized && h.revisionStore != nil && cib.AnalysisRevisionID != "" {
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
