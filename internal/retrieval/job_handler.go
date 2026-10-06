package retrieval

import (
	"context"
	"fmt"
	"strconv"

	"repolens/internal/analysispipeline"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
	"repolens/internal/snapshot"
)

// RetrievalJobHandler processes BUILD_RETRIEVAL jobs.
type RetrievalJobHandler struct {
	ciStore       codeintelstore.Store
	publisher     retrievalArtifactPublisher
	snapshotStore snapshot.Store
	storeFS       snapshotstore.SnapshotStore
	finalizer     *analysispipeline.Finalizer
}

type retrievalArtifactPublisher interface {
	Publish(buildID int64, executionGeneration int64, claimToken string, strategy string, idx *bm25.Index) (string, string, error)
}

func (h *RetrievalJobHandler) WithFinalizer(finalizer *analysispipeline.Finalizer) *RetrievalJobHandler {
	h.finalizer = finalizer
	return h
}

// WithSnapshotSource configures immutable snapshot reads for indexing symbol
// source bodies into the retrieval artifact.
func (h *RetrievalJobHandler) WithSnapshotSource(snapStore snapshot.Store, storeFS snapshotstore.SnapshotStore) *RetrievalJobHandler {
	h.snapshotStore = snapStore
	h.storeFS = storeFS
	return h
}

// NewRetrievalJobHandler creates a new handler for BUILD_RETRIEVAL jobs.
func NewRetrievalJobHandler(ciStore codeintelstore.Store, baseStorageDir string) *RetrievalJobHandler {
	return &RetrievalJobHandler{
		ciStore:   ciStore,
		publisher: artifact.NewPublisher(baseStorageDir),
	}
}

// Execute builds and atomically publishes the BM25 retrieval index for a RetrievalBuild.
func (h *RetrievalJobHandler) Execute(ctx context.Context, job *jobs.AnalysisJob) error {
	if job == nil || job.WorkerID == nil || job.ClaimToken == nil || *job.WorkerID == "" || *job.ClaimToken == "" {
		return jobs.ErrOwnershipLost
	}
	log := logger.L(ctx)
	rbID, err := strconv.ParseInt(job.ResourceID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid retrieval build resource ID %s: %w", job.ResourceID, err)
	}
	rb, err := h.ciStore.GetRetrievalBuildByID(ctx, rbID)
	if err != nil {
		return fmt.Errorf("failed fetching retrieval build %d: %w", rbID, err)
	}
	if rb.Status == codeintelmodel.BuildStatusReady {
		return nil
	}

	cib, err := h.ciStore.GetByID(ctx, rb.CodeIndexBuildID)
	if err != nil {
		return fmt.Errorf("failed fetching code index build %d for retrieval: %w", rb.CodeIndexBuildID, err)
	}
	if cib.Status != codeintelmodel.BuildStatusReady {
		return jobs.NewRetryableError("CODE_INDEX_NOT_READY", "code index build is not READY", nil)
	}
	if err := h.ciStore.MarkRetrievalBuilding(ctx, rb.ID); err != nil {
		return jobs.NewRetryableError("RETRIEVAL_STATE_UPDATE_FAILED", err.Error(), err)
	}
	if (h.snapshotStore == nil) != (h.storeFS == nil) {
		return fmt.Errorf("snapshot source requires both snapshot metadata and filesystem stores")
	}
	var sourceSnapshot *snapshot.RepositorySnapshot
	if h.snapshotStore != nil {
		sourceSnapshot, err = h.snapshotStore.GetByID(ctx, cib.SnapshotID)
		if err != nil {
			return fmt.Errorf("failed fetching source snapshot %s for retrieval build: %w", cib.SnapshotID, err)
		}
		if sourceSnapshot.Status != snapshot.StatusReady {
			return jobs.NewRetryableError("SNAPSHOT_NOT_READY", "source snapshot is not READY", nil)
		}
	}

	log.Info("starting retrieval build indexing", "retrieval_build_id", rb.ID, "code_index_build_id", cib.ID)

	symbols, err := h.ciStore.ListAllSymbols(ctx, cib.ID)
	if err != nil {
		return fmt.Errorf("failed listing symbols for retrieval build: %w", err)
	}
	var source *SymbolIndexSource
	if sourceSnapshot != nil {
		source = &SymbolIndexSource{Store: h.storeFS, RepositoryID: sourceSnapshot.RepositoryID, SnapshotID: sourceSnapshot.ID}
	}
	idx, err := BuildSymbolIndex(ctx, symbols, source)
	if err != nil {
		return err
	}

	claimToken := ""
	if job.ClaimToken != nil {
		claimToken = *job.ClaimToken
	}
	executionGeneration := job.ExecutionGeneration
	if executionGeneration < 1 {
		executionGeneration = 1
	}
	finalPath, artifactHash, err := h.publisher.Publish(rb.ID, int64(executionGeneration), claimToken, rb.Strategy, idx)
	if err != nil {
		log.Error("failed publishing retrieval artifact", "build_id", rb.ID, "error", err)
		return err
	}

	var finalizeErr error
	if rb.AnalysisRevisionID != "" {
		if h.finalizer == nil {
			return fmt.Errorf("analysis pipeline finalizer is not configured")
		}
		finalizeErr = h.finalizer.FinalizeRetrieval(ctx, analysispipeline.RetrievalStageResult{
			Ownership:  analysispipeline.JobOwnership{JobID: job.ID, WorkerID: *job.WorkerID, ClaimToken: *job.ClaimToken},
			RevisionID: rb.AnalysisRevisionID, RetrievalBuildID: rb.ID,
			ArtifactPath: finalPath, ArtifactHash: artifactHash, DocCount: idx.TotalDocs,
		})
	} else {
		finalizeErr = h.ciStore.FinalizeRetrievalSuccess(ctx, job.ID, *job.WorkerID, *job.ClaimToken, rb.ID, finalPath, artifactHash, idx.TotalDocs)
	}
	if finalizeErr != nil {
		log.Error("failed updating retrieval build to READY", "build_id", rb.ID, "error", finalizeErr)
		return jobs.WrapAtomicHandlerFinalization(jobs.StatusSucceeded, finalizeErr)
	}
	log.Info("retrieval build completed and published successfully",
		"build_id", rb.ID,
		"docs", idx.TotalDocs,
		"artifact_path", finalPath,
		"hash", artifactHash,
	)

	return nil
}
