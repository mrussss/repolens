package retrieval

import (
	"context"
	"fmt"
	"strconv"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
)

// RetrievalJobHandler processes BUILD_RETRIEVAL jobs.
type RetrievalJobHandler struct {
	ciStore   codeintelstore.Store
	publisher *artifact.Publisher
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

	log.Info("starting retrieval build indexing", "retrieval_build_id", rb.ID, "code_index_build_id", cib.ID)

	symbols, err := h.ciStore.ListSymbols(ctx, cib.ID, "", 10000)
	if err != nil {
		if job.AttemptCount >= job.MaxAttempts {
			_ = h.ciStore.FailRetrievalBuild(ctx, rb.ID, err.Error())
		}
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
		if job.AttemptCount >= job.MaxAttempts {
			_ = h.ciStore.FailRetrievalBuild(ctx, rb.ID, err.Error())
		}
		return err
	}

	if err := h.ciStore.CompleteRetrievalBuild(ctx, rb.ID, finalPath, artifactHash, idx.TotalDocs); err != nil {
		log.Error("failed updating retrieval build to READY", "build_id", rb.ID, "error", err)
		return err
	}

	log.Info("retrieval build completed and published successfully",
		"build_id", rb.ID,
		"docs", idx.TotalDocs,
		"artifact_path", finalPath,
		"hash", artifactHash,
	)

	return nil
}
