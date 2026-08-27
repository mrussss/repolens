package indexing

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
)

// SnapshotJobHandler handles MATERIALIZE_SNAPSHOT jobs.
type SnapshotJobHandler struct {
	repoStore      repo.Store
	snapshotStore  snapshot.Store
	indexStore     repoindex.Store
	codeIntelStore codeintelstore.Store
	storeFS        snapshotstore.SnapshotStore
	cloner         GitCloner
	filter         *FileFilter
	chunker        *CodeChunker
	indexWriter    ChunkIndexWriter
	maxRepoBytes   int64
	maxFileCount   int
}

func (h *SnapshotJobHandler) WithResourceLimits(maxRepoBytes int64, maxFileCount int) *SnapshotJobHandler {
	h.maxRepoBytes = maxRepoBytes
	h.maxFileCount = maxFileCount
	return h
}

type GitCloner interface {
	CloneTo(ctx context.Context, gitURL, ref, targetDir string) (string, error)
	ValidateGitURL(rawURL string) error
}

// NewSnapshotJobHandler creates a new SnapshotJobHandler.
func NewSnapshotJobHandler(
	repoStore repo.Store,
	snapshotStore snapshot.Store,
	indexStore repoindex.Store,
	storeFS snapshotstore.SnapshotStore,
	cloner GitCloner,
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

// WithCodeIntelStore equips the handler with CodeIntelStore for automatic build chaining.
func (h *SnapshotJobHandler) WithCodeIntelStore(cis codeintelstore.Store) *SnapshotJobHandler {
	h.codeIntelStore = cis
	return h
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
	if snap.Status == snapshot.StatusCreated {
		if err := h.snapshotStore.UpdateStatus(ctx, snap.ID, snapshot.StatusCreated, snapshot.StatusMaterializing, nil); err != nil {
			return jobs.NewRetryableError("SNAPSHOT_STATE_UPDATE_FAILED", err.Error(), err)
		}
		snap.Status = snapshot.StatusMaterializing
	}

	r, err := h.repoStore.GetByID(ctx, snap.RepositoryID)
	if err != nil {
		return jobs.NewPermanentError("REPO_NOT_FOUND", fmt.Sprintf("repository %s not found: %v", snap.RepositoryID, err), err)
	}

	targetDir := h.storeFS.GetSourcePath(snap.RepositoryID, snap.ID)
	commitSHA := snap.CommitSHA
	if commitSHA == "pending" || commitSHA == "" || !sourceDirectoryExists(targetDir) {
		// Publish a fully cloned tree only after git has resolved HEAD.  A
		// retry therefore cannot expose a half-written source directory.
		stagingDir := targetDir + ".tmp-" + jobClaimSuffix(job)
		_ = os.RemoveAll(stagingDir)
		cloneSHA, cloneErr := h.cloner.CloneTo(ctx, r.GitURL, snap.Ref, stagingDir)
		if cloneErr != nil {
			log.Error("failed to clone repository for snapshot", "error", cloneErr)
			if err := h.cloner.ValidateGitURL(r.GitURL); err != nil {
				h.failIfTerminal(ctx, job, snap.ID, "INVALID_GIT_URL")
				return jobs.NewPermanentError("INVALID_GIT_URL", err.Error(), err)
			}
			h.failIfTerminal(ctx, job, snap.ID, "CLONE_FAILED")
			return jobs.NewRetryableError("CLONE_FAILED", cloneErr.Error(), cloneErr)
		}
		if snap.CommitSHA != "pending" && snap.CommitSHA != "" && cloneSHA != snap.CommitSHA {
			h.failIfTerminal(ctx, job, snap.ID, "COMMIT_CHANGED_DURING_RESOLVE")
			return jobs.NewPermanentError("COMMIT_CHANGED_DURING_RESOLVE", fmt.Sprintf("resolved %s but clone returned %s", snap.CommitSHA, cloneSHA), nil)
		}
		if err := os.RemoveAll(targetDir); err != nil {
			return jobs.NewRetryableError("SNAPSHOT_PUBLISH_FAILED", err.Error(), err)
		}
		if err := os.Rename(stagingDir, targetDir); err != nil {
			return jobs.NewRetryableError("SNAPSHOT_PUBLISH_FAILED", err.Error(), err)
		}
		commitSHA = cloneSHA
	}

	// Build chunks & index if indexWriter is provided (migration compatibility)
	var allChunks []CodeChunk
	docCount := 0
	fileCount := 0
	var totalBytes int64
	manifest := make([]string, 0)

	walkErr := h.storeFS.WalkFiles(snap.RepositoryID, snap.ID, func(relPath string, info os.FileInfo) error {
		if info.IsDir() {
			if h.filter.ShouldIgnoreDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if h.filter.IsOversized(info.Size()) {
			return jobs.NewPermanentError("FILE_TOO_LARGE", fmt.Sprintf("file %s exceeds maximum size", relPath), nil)
		}
		if h.filter.ShouldIgnoreFile(relPath, info.Size()) {
			return nil
		}
		if h.maxFileCount > 0 && fileCount >= h.maxFileCount {
			return jobs.NewPermanentError("TOO_MANY_FILES", fmt.Sprintf("repository exceeds maximum file count %d", h.maxFileCount), nil)
		}
		if h.maxRepoBytes > 0 && totalBytes+info.Size() > h.maxRepoBytes {
			return jobs.NewPermanentError("REPOSITORY_TOO_LARGE", fmt.Sprintf("repository exceeds maximum size %d bytes", h.maxRepoBytes), nil)
		}

		content, err := h.storeFS.ReadFile(ctx, snap.RepositoryID, snap.ID, relPath, 1, -1)
		if err != nil {
			return err
		}

		chunks := h.chunker.ChunkFile(snap.ID, relPath, content)
		allChunks = append(allChunks, chunks...)
		docCount++
		fileCount++
		totalBytes += info.Size()
		manifest = append(manifest, relPath+"\x00"+string(content))
		return nil
	})

	if walkErr != nil {
		log.Error("failed to walk snapshot files", "error", walkErr)
		h.failIfTerminal(ctx, job, snap.ID, "WALK_FAILED")
		if class, _ := jobs.ClassifyError(walkErr); class == jobs.ErrorClassPermanent {
			return walkErr
		}
		return jobs.NewRetryableError("WALK_FAILED", walkErr.Error(), walkErr)
	}

	sort.Strings(manifest)
	hasher := sha256.New()
	for _, entry := range manifest {
		_, _ = hasher.Write([]byte(entry))
	}
	contentHash := fmt.Sprintf("%x", hasher.Sum(nil))

	if h.indexWriter != nil && len(allChunks) > 0 {
		if err := h.indexWriter.IndexChunks(ctx, snap.ID, allChunks); err != nil {
			log.Error("failed writing chunks to index store", "error", err)
			h.failIfTerminal(ctx, job, snap.ID, "INDEX_WRITE_FAILED")
			return jobs.NewRetryableError("INDEX_WRITE_FAILED", err.Error(), err)
		}
	}

	now := time.Now().UTC()
	if err := sealSnapshot(targetDir); err != nil {
		h.failIfTerminal(ctx, job, snap.ID, "SNAPSHOT_SEAL_FAILED")
		return jobs.NewRetryableError("SNAPSHOT_SEAL_FAILED", err.Error(), err)
	}
	if finalizer, ok := h.snapshotStore.(snapshot.ClaimedMaterializationFinalizer); ok && job.WorkerID != nil && job.ClaimToken != nil {
		if err := finalizer.FinalizeSnapshotSuccess(ctx, job.ID, *job.WorkerID, *job.ClaimToken, snap.ID, commitSHA, contentHash, fileCount, totalBytes, now); err != nil {
			h.failIfTerminal(ctx, job, snap.ID, "SNAPSHOT_FINALIZE_FAILED")
			return err
		}
	} else if finalizer, ok := h.snapshotStore.(snapshot.MaterializationFinalizer); ok {
		if err := finalizer.FinalizeMaterialization(ctx, snap.ID, commitSHA, contentHash, fileCount, totalBytes, now); err != nil {
			h.failIfTerminal(ctx, job, snap.ID, "SNAPSHOT_FINALIZE_FAILED")
			return jobs.NewRetryableError("SNAPSHOT_FINALIZE_FAILED", err.Error(), err)
		}
	} else if err := h.snapshotStore.UpdateStatus(ctx, snap.ID, snapshot.StatusMaterializing, snapshot.StatusReady, &now); err != nil {
		return jobs.NewRetryableError("SNAPSHOT_FINALIZE_FAILED", err.Error(), err)
	}

	// Auto-chain BUILD_CODE_INDEX job if codeIntelStore is wired
	if h.codeIntelStore != nil {
		_, _, _ = h.codeIntelStore.GetOrCreateBuild(ctx, snap.ID, r.Name, codeintelmodel.DefaultBuildContext())
	}

	log.Info("snapshot materialization completed successfully", "chunks", len(allChunks), "docs", docCount)
	return nil
}

func sealSnapshot(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0555)
		}
		return os.Chmod(path, 0444)
	})
}

func sourceDirectoryExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func (h *SnapshotJobHandler) failIfTerminal(ctx context.Context, job *jobs.AnalysisJob, snapshotID, code string) {
	if _, claimed := h.snapshotStore.(snapshot.ClaimedMaterializationFinalizer); claimed {
		// Production terminal transitions are performed by the claim-fenced
		// AnalysisJob finalizer. This compatibility helper is for lightweight
		// in-memory test stores only.
		return
	}
	if job != nil && (job.AttemptCount >= job.MaxAttempts) {
		if finalizer, ok := h.snapshotStore.(snapshot.MaterializationFinalizer); ok {
			_ = finalizer.FailMaterialization(ctx, snapshotID, code)
		} else {
			_ = h.snapshotStore.UpdateStatus(ctx, snapshotID, snapshot.StatusMaterializing, snapshot.StatusFailed, nil)
		}
	}
}

func jobClaimSuffix(job *jobs.AnalysisJob) string {
	if job != nil && job.ClaimToken != nil && *job.ClaimToken != "" {
		return (*job.ClaimToken)[:minInt(12, len(*job.ClaimToken))]
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
