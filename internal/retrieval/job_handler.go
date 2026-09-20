package retrieval

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
	"repolens/internal/revision"
)

// RetrievalJobHandler processes BUILD_RETRIEVAL jobs.
type RetrievalJobHandler struct {
	ciStore       codeintelstore.Store
	publisher     *artifact.Publisher
	revisionStore interface {
		MarkRetrievalReady(context.Context, string, int64) error
		MarkFailed(context.Context, string, revision.Stage, string, string) error
	}
}

// NewRetrievalJobHandler creates a new handler for BUILD_RETRIEVAL jobs.
func NewRetrievalJobHandler(ciStore codeintelstore.Store, baseStorageDir string) *RetrievalJobHandler {
	return &RetrievalJobHandler{
		ciStore:   ciStore,
		publisher: artifact.NewPublisher(baseStorageDir),
	}
}

func (h *RetrievalJobHandler) WithRevisionStore(store interface {
	MarkRetrievalReady(context.Context, string, int64) error
	MarkFailed(context.Context, string, revision.Stage, string, string) error
}) *RetrievalJobHandler {
	h.revisionStore = store
	return h
}

// Execute builds and atomically publishes the BM25 retrieval index for a RetrievalBuild.
func (h *RetrievalJobHandler) Execute(ctx context.Context, job *jobs.AnalysisJob) (executeErr error) {
	log := logger.L(ctx)
	rbID, err := strconv.ParseInt(job.ResourceID, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid retrieval build resource ID %s: %w", job.ResourceID, err)
	}
	defer func() {
		if executeErr != nil && h.revisionStore != nil && job.AttemptCount >= job.MaxAttempts {
			if build, buildErr := h.ciStore.GetRetrievalBuildByID(context.Background(), rbID); buildErr == nil && build.AnalysisRevisionID != "" {
				_ = h.revisionStore.MarkFailed(context.Background(), build.AnalysisRevisionID, revision.StageBuildingSearch, "RETRIEVAL_BUILD_FAILED", executeErr.Error())
			}
		}
	}()

	rb, err := h.ciStore.GetRetrievalBuildByID(ctx, rbID)
	if err != nil {
		return fmt.Errorf("failed fetching retrieval build %d: %w", rbID, err)
	}
	if rb.Status == codeintelmodel.BuildStatusReady {
		if h.revisionStore != nil && rb.AnalysisRevisionID != "" {
			if err := h.revisionStore.MarkRetrievalReady(ctx, rb.AnalysisRevisionID, rb.ID); err != nil && !errors.Is(err, revision.ErrLineage) {
				return jobs.NewRetryableError("REVISION_STAGE_UPDATE_FAILED", err.Error(), err)
			}
		}
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

	log.Info("starting retrieval build indexing", "retrieval_build_id", rb.ID, "code_index_build_id", cib.ID)

	symbols, err := h.ciStore.ListSymbols(ctx, cib.ID, "", 10000)
	if err != nil {
		return fmt.Errorf("failed listing symbols for retrieval build: %w", err)
	}

	idx := bm25.NewIndex(1.2, 0.75)
	for _, sym := range symbols {
		content := fmt.Sprintf("%s %s %s %s %s", sym.Name, sym.QualifiedName, sym.ReceiverCanonical, sym.Signature, sym.Doc)
		idx.AddDocument(bm25.Document{
			FilePath:      sym.FilePath,
			StartLine:     sym.StartLine,
			EndLine:       sym.EndLine,
			Content:       content,
			SymbolKeyHash: sym.SymbolKeyHash,
			SymbolName:    sym.Name,
			Kind:          string(sym.Kind),
		})
	}
	idx.Build()

	claimToken := ""
	if job.ClaimToken != nil {
		claimToken = *job.ClaimToken
	}

	finalPath, artifactHash, err := h.publisher.Publish(rb.ID, claimToken, rb.Strategy, idx)
	if err != nil {
		log.Error("failed publishing retrieval artifact", "build_id", rb.ID, "error", err)
		return err
	}

	var finalizeErr error
	stageFinalized := false
	if finalizer, ok := h.ciStore.(interface {
		FinalizeRetrievalSuccessWithRevision(context.Context, int64, string, string, int64, string, string, string, int) error
	}); ok && job.WorkerID != nil && job.ClaimToken != nil && rb.AnalysisRevisionID != "" {
		finalizeErr = finalizer.FinalizeRetrievalSuccessWithRevision(ctx, job.ID, *job.WorkerID, *job.ClaimToken, rb.ID, rb.AnalysisRevisionID, finalPath, artifactHash, idx.TotalDocs)
		stageFinalized = finalizeErr == nil
	} else if finalizer, ok := h.ciStore.(interface {
		FinalizeRetrievalSuccess(context.Context, int64, string, string, int64, string, string, int) error
	}); ok && job.WorkerID != nil && job.ClaimToken != nil {
		finalizeErr = finalizer.FinalizeRetrievalSuccess(ctx, job.ID, *job.WorkerID, *job.ClaimToken, rb.ID, finalPath, artifactHash, idx.TotalDocs)
	} else {
		finalizeErr = h.ciStore.CompleteRetrievalBuild(ctx, rb.ID, finalPath, artifactHash, idx.TotalDocs)
	}
	if finalizeErr != nil {
		log.Error("failed updating retrieval build to READY", "build_id", rb.ID, "error", finalizeErr)
		return finalizeErr
	}
	if !stageFinalized && h.revisionStore != nil && rb.AnalysisRevisionID != "" {
		if err := h.revisionStore.MarkRetrievalReady(ctx, rb.AnalysisRevisionID, rb.ID); err != nil {
			return jobs.NewRetryableError("REVISION_STAGE_UPDATE_FAILED", err.Error(), err)
		}
	}

	log.Info("retrieval build completed and published successfully",
		"build_id", rb.ID,
		"docs", idx.TotalDocs,
		"artifact_path", finalPath,
		"hash", artifactHash,
	)

	return nil
}
