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
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/retrieval/bm25"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

const maxBM25SymbolSourceBytes = 64 * 1024

// RetrievalJobHandler processes BUILD_RETRIEVAL jobs.
type RetrievalJobHandler struct {
	ciStore       codeintelstore.Store
	publisher     *artifact.Publisher
	snapshotStore snapshot.Store
	storeFS       snapshotstore.SnapshotStore
	revisionStore interface {
		MarkRetrievalReady(context.Context, string, int64) error
		MarkFailed(context.Context, string, revision.Stage, string, string) error
	}
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
	var sourceBodies []string
	if sourceSnapshot != nil {
		sourceBodies, err = readSymbolSourceBodies(ctx, h.storeFS, sourceSnapshot.RepositoryID, sourceSnapshot.ID, symbols)
		if err != nil {
			return fmt.Errorf("failed reading pinned snapshot source bodies: %w", err)
		}
	}

	idx := bm25.NewIndex(1.2, 0.75)
	for symbolIndex, sym := range symbols {
		content := fmt.Sprintf("%s %s %s %s %s", sym.Name, sym.QualifiedName, sym.ReceiverCanonical, sym.Signature, sym.Doc)
		if sourceSnapshot != nil {
			content += "\n" + sourceBodies[symbolIndex]
		}
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

func readSymbolSourceBodies(ctx context.Context, storeFS snapshotstore.SnapshotStore, repoID, snapshotID string, symbols []*codeintelmodel.Symbol) ([]string, error) {
	bodies := make([]string, len(symbols))
	byPath := make(map[string][]int)
	for i, sym := range symbols {
		if sym.StartLine < 1 || sym.EndLine < sym.StartLine {
			return nil, fmt.Errorf("symbol %s has invalid source range %d-%d", sym.Name, sym.StartLine, sym.EndLine)
		}
		byPath[sym.FilePath] = append(byPath[sym.FilePath], i)
	}

	if batchReader, ok := storeFS.(interface {
		ReadFileRangesBounded(context.Context, string, string, string, []snapshotstore.LineRange, int) ([]snapshotstore.BoundedLineRangeResult, error)
	}); ok {
		for path, indexes := range byPath {
			ranges := make([]snapshotstore.LineRange, len(indexes))
			for i, symbolIndex := range indexes {
				sym := symbols[symbolIndex]
				ranges[i] = snapshotstore.LineRange{StartLine: sym.StartLine, EndLine: sym.EndLine}
			}
			results, err := batchReader.ReadFileRangesBounded(ctx, repoID, snapshotID, path, ranges, maxBM25SymbolSourceBytes)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", path, err)
			}
			if len(results) != len(indexes) {
				return nil, fmt.Errorf("read %s returned %d ranges, want %d", path, len(results), len(indexes))
			}
			for i, result := range results {
				if result.Err != nil && !errors.Is(result.Err, snapshotstore.ErrLineTooLong) {
					return nil, fmt.Errorf("symbol %s in %s: %w", symbols[indexes[i]].Name, path, result.Err)
				}
				if result.Err == nil {
					bodies[indexes[i]] = result.Content
				}
			}
		}
		return bodies, nil
	}

	for i, sym := range symbols {
		body, err := readSymbolSourceBody(ctx, storeFS, repoID, snapshotID, sym)
		if err != nil {
			return nil, fmt.Errorf("symbol %s in %s: %w", sym.Name, sym.FilePath, err)
		}
		bodies[i] = body
	}
	return bodies, nil
}

func readSymbolSourceBody(ctx context.Context, storeFS snapshotstore.SnapshotStore, repoID, snapshotID string, sym *codeintelmodel.Symbol) (string, error) {
	if sym.StartLine < 1 || sym.EndLine < sym.StartLine {
		return "", fmt.Errorf("invalid source range %d-%d", sym.StartLine, sym.EndLine)
	}
	var content string
	var err error
	if bounded, ok := storeFS.(interface {
		ReadFileRangeBounded(context.Context, string, string, string, int, int, int) (snapshotstore.FileRange, error)
	}); ok {
		var file snapshotstore.FileRange
		file, err = bounded.ReadFileRangeBounded(ctx, repoID, snapshotID, sym.FilePath, sym.StartLine, sym.EndLine, maxBM25SymbolSourceBytes)
		content = file.Content
	} else {
		var file snapshotstore.FileRange
		file, err = storeFS.ReadFileRange(ctx, repoID, snapshotID, sym.FilePath, sym.StartLine, sym.EndLine, maxBM25SymbolSourceBytes)
		content = file.Content
	}
	if errors.Is(err, snapshotstore.ErrLineTooLong) {
		// One pathological line should not fail the whole build. Metadata and
		// documentation still form a searchable document for this symbol.
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return content, nil
}
