package indexing

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
)

// SnapshotJobHandler handles MATERIALIZE_SNAPSHOT jobs.
type SnapshotJobHandler struct {
	repoStore     repo.Store
	snapshotStore snapshot.Store
	indexStore    repoindex.Store
	storeFS       snapshotstore.SnapshotStore
	cloner        *SafeGitCloner
	filter        *FileFilter
	chunker       *CodeChunker
	indexWriter   ChunkIndexWriter
}

// NewSnapshotJobHandler creates a new SnapshotJobHandler.
func NewSnapshotJobHandler(
	repoStore repo.Store,
	snapshotStore snapshot.Store,
	indexStore repoindex.Store,
	storeFS snapshotstore.SnapshotStore,
	cloner *SafeGitCloner,
	filter *FileFilter,
	chunker *CodeChunker,
	indexWriter ChunkIndexWriter,
) *SnapshotJobHandler {
	return &SnapshotJobHandler{
		repoStore:     repoStore,
		snapshotStore: snapshotStore,
		indexStore:    indexStore,
		storeFS:       storeFS,
		cloner:        cloner,
		filter:        filter,
		chunker:       chunker,
		indexWriter:   indexWriter,
	}
}

// Execute processes a MATERIALIZE_SNAPSHOT job.
func (h *SnapshotJobHandler) Execute(ctx context.Context, job *jobs.AnalysisJob) error {
	snapID := job.ResourceID
	log := logger.L(ctx).With("snapshot_id", snapID, "job_id", job.ID)

	snap, err := h.snapshotStore.GetByID(ctx, snapID)
	if err != nil {
		return jobs.NewPermanentError("SNAPSHOT_NOT_FOUND", fmt.Sprintf("snapshot %s not found: %v", snapID, err), err)
	}

	if snap.Status == snapshot.StatusReady {
		log.Info("snapshot already READY")
		return nil
	}

	r, err := h.repoStore.GetByID(ctx, snap.RepositoryID)
	if err != nil {
		return jobs.NewPermanentError("REPO_NOT_FOUND", fmt.Sprintf("repository %s not found: %v", snap.RepositoryID, err), err)
	}

	targetDir := h.storeFS.GetSourcePath(snap.RepositoryID, snap.ID)
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		_, cloneErr := h.cloner.CloneTo(ctx, r.GitURL, snap.Ref, targetDir)
		if cloneErr != nil {
			log.Error("failed to clone repository for snapshot", "error", cloneErr)
			_ = h.snapshotStore.UpdateStatus(ctx, snap.ID, snapshot.StatusMaterializing, snapshot.StatusMaterializeFailed, nil)
			return jobs.NewPermanentError("CLONE_FAILED", cloneErr.Error(), cloneErr)
		}
	}

	now := time.Now().UTC()
	_ = h.snapshotStore.UpdateStatus(ctx, snap.ID, snapshot.StatusMaterializing, snapshot.StatusReady, &now)

	// Build chunks & index if indexWriter is provided
	var allChunks []CodeChunk
	docCount := 0

	walkErr := h.storeFS.WalkFiles(snap.RepositoryID, snap.ID, func(relPath string, info os.FileInfo) error {
		if info.IsDir() {
			if h.filter.ShouldIgnoreDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if h.filter.ShouldIgnoreFile(relPath, info.Size()) {
			return nil
		}

		content, err := h.storeFS.ReadFile(ctx, snap.RepositoryID, snap.ID, relPath, 1, -1)
		if err != nil {
			return nil
		}

		chunks := h.chunker.ChunkFile(snap.ID, relPath, content)
		allChunks = append(allChunks, chunks...)
		docCount++
		return nil
	})

	if walkErr != nil {
		log.Error("failed to walk snapshot files", "error", walkErr)
		return jobs.NewRetryableError("WALK_FAILED", walkErr.Error(), walkErr)
	}

	hasher := sha256.New()
	for _, ch := range allChunks {
		hasher.Write([]byte(ch.ContentHash))
	}

	if h.indexWriter != nil && len(allChunks) > 0 {
		if err := h.indexWriter.IndexChunks(ctx, snap.ID, allChunks); err != nil {
			log.Error("failed writing chunks to index store", "error", err)
			return jobs.NewRetryableError("INDEX_WRITE_FAILED", err.Error(), err)
		}
	}

	log.Info("snapshot materialization completed successfully", "chunks", len(allChunks), "docs", docCount)
	return nil
}
