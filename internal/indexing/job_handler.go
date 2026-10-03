package indexing

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"repolens/internal/analysispipeline"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
	"repolens/internal/snapshotpolicy"
)

// SnapshotJobHandler handles MATERIALIZE_SNAPSHOT jobs.
type SnapshotJobHandler struct {
	repoStore               repo.Store
	snapshotStore           snapshot.Store
	indexStore              repoindex.Store
	codeIntelStore          codeintelstore.Store
	storeFS                 snapshotstore.SnapshotStore
	cloner                  GitCloner
	filter                  *FileFilter
	chunker                 *CodeChunker
	indexWriter             ChunkIndexWriter
	maxIndexableSourceBytes int64
	maxFileCount            int
	revisionStore           interface {
		MarkSnapshotReady(context.Context, string, string) error
	}
	finalizer *analysispipeline.Finalizer
}

func (h *SnapshotJobHandler) WithResourceLimits(maxRepoBytes int64, maxFileCount int) *SnapshotJobHandler {
	h.maxIndexableSourceBytes = maxRepoBytes
	h.maxFileCount = maxFileCount
	return h
}

type GitCloner interface {
	CloneTo(ctx context.Context, gitURL, ref, targetDir string) (string, error)
	ValidateGitURL(rawURL string) error
}

type ExactGitCloner interface {
	CloneCommitTo(ctx context.Context, gitURL, commitSHA, targetDir string) (string, error)
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

// WithRevisionStore connects the product-level revision state to the
// existing snapshot job while keeping the DB-backed job pipeline unchanged.
func (h *SnapshotJobHandler) WithRevisionStore(store interface {
	MarkSnapshotReady(context.Context, string, string) error
}) *SnapshotJobHandler {
	h.revisionStore = store
	return h
}

func (h *SnapshotJobHandler) WithFinalizer(finalizer *analysispipeline.Finalizer) *SnapshotJobHandler {
	h.finalizer = finalizer
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
		if h.finalizer == nil && h.revisionStore != nil && snap.AnalysisRevisionID != "" {
			if err := h.revisionStore.MarkSnapshotReady(ctx, snap.AnalysisRevisionID, snap.ID); err != nil && !errors.Is(err, revision.ErrLineage) {
				return jobs.NewRetryableError("REVISION_STAGE_UPDATE_FAILED", err.Error(), err)
			}
		}
		if h.codeIntelStore != nil && snap.AnalysisRevisionID == "" {
			return h.createLegacyCodeIndexHandoff(ctx, snap)
		}
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
	executionGeneration := job.ExecutionGeneration
	if executionGeneration < 1 {
		executionGeneration = 1
	}
	claimToken := jobExecutionClaimToken(job)
	if executionStore, ok := h.storeFS.(interface {
		GetExecutionSourcePath(string, string, int, string) (string, error)
	}); ok {
		targetDir, err = executionStore.GetExecutionSourcePath(snap.RepositoryID, snap.ID, executionGeneration, claimToken)
		if err != nil {
			return jobs.NewPermanentError("SNAPSHOT_PATH_INVALID", err.Error(), err)
		}
	}
	commitSHA := snap.CommitSHA
	if commitSHA == "pending" || commitSHA == "" || !sourceDirectoryExists(targetDir) {
		// Publish a fully cloned tree only after git has resolved HEAD.  A
		// retry therefore cannot expose a half-written source directory.
		if err := os.MkdirAll(filepath.Dir(targetDir), 0755); err != nil {
			return jobs.NewRetryableError("SNAPSHOT_PUBLISH_FAILED", err.Error(), err)
		}
		stageRoot := filepath.Dir(filepath.Dir(targetDir))
		stagingDir, stageErr := os.MkdirTemp(stageRoot, ".snapshot-stage-")
		if stageErr != nil {
			return jobs.NewRetryableError("SNAPSHOT_PUBLISH_FAILED", stageErr.Error(), stageErr)
		}
		defer os.RemoveAll(stagingDir)
		cloneSHA, cloneErr := "", error(nil)
		if exactCloner, ok := h.cloner.(ExactGitCloner); ok && snap.CommitSHA != "" && snap.CommitSHA != "pending" {
			cloneSHA, cloneErr = exactCloner.CloneCommitTo(ctx, r.GitURL, snap.CommitSHA, stagingDir)
		} else {
			cloneSHA, cloneErr = h.cloner.CloneTo(ctx, r.GitURL, snap.Ref, stagingDir)
		}
		if cloneErr != nil {
			log.Error("failed to clone repository for snapshot", "error", cloneErr)
			if errors.Is(cloneErr, ErrRepositoryCloneSizeLimit) {
				h.failIfTerminal(ctx, job, snap.ID, "REPOSITORY_CLONE_SIZE_LIMIT")
				return jobs.NewPermanentError("REPOSITORY_CLONE_SIZE_LIMIT", cloneErr.Error(), cloneErr)
			}
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
		if _, statErr := os.Lstat(targetDir); statErr == nil {
			return jobs.NewRetryableError("SNAPSHOT_PUBLISH_FAILED", "immutable execution path already exists", nil)
		} else if !os.IsNotExist(statErr) {
			return jobs.NewRetryableError("SNAPSHOT_PUBLISH_FAILED", statErr.Error(), statErr)
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
	manifestFiles := make([]snapshotpolicy.FileEntry, 0)

	walkFiles := h.storeFS.WalkFiles
	readFile := h.storeFS.ReadFile
	if executionStore, ok := h.storeFS.(interface {
		WalkFilesAt(string, func(string, os.FileInfo) error) error
		ReadFileAt(context.Context, string, string, int, int) (string, error)
	}); ok {
		walkFiles = func(_, _ string, fn func(string, os.FileInfo) error) error {
			return executionStore.WalkFilesAt(targetDir, fn)
		}
		readFile = func(ctx context.Context, _, _, path string, start, end int) (string, error) {
			return executionStore.ReadFileAt(ctx, targetDir, path, start, end)
		}
	}
	walkErr := walkFiles(snap.RepositoryID, snap.ID, func(relPath string, info os.FileInfo) error {
		if info.IsDir() {
			if h.filter.ShouldIgnoreDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if h.filter.IsExplicitlyIgnoredFile(relPath) {
			return nil
		}
		if h.filter.IsOversized(info.Size()) {
			return jobs.NewPermanentError("FILE_TOO_LARGE", fmt.Sprintf("file %s exceeds maximum size", relPath), nil)
		}
		if h.maxFileCount > 0 && fileCount >= h.maxFileCount {
			return jobs.NewPermanentError("TOO_MANY_FILES", fmt.Sprintf("repository exceeds maximum file count %d", h.maxFileCount), nil)
		}
		if h.maxIndexableSourceBytes > 0 && totalBytes+info.Size() > h.maxIndexableSourceBytes {
			return jobs.NewPermanentError("REPOSITORY_TOO_LARGE", fmt.Sprintf("indexable source exceeds maximum size %d bytes", h.maxIndexableSourceBytes), nil)
		}

		content, err := readFile(ctx, snap.RepositoryID, snap.ID, relPath, 1, -1)
		if err != nil {
			return err
		}

		chunks := h.chunker.ChunkFile(snap.ID, relPath, content)
		allChunks = append(allChunks, chunks...)
		manifestFiles = append(manifestFiles, snapshotpolicy.FileEntryFor(relPath, []byte(content)))
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
	fileManifest := snapshotpolicy.NewManifest(snap.ID, commitSHA, contentHash, manifestFiles)
	if err := snapshotpolicy.WriteManifest(targetDir, fileManifest); err != nil {
		h.failIfTerminal(ctx, job, snap.ID, "SNAPSHOT_MANIFEST_WRITE_FAILED")
		return jobs.NewRetryableError("SNAPSHOT_MANIFEST_WRITE_FAILED", err.Error(), err)
	}

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
	if err := os.Chmod(snapshotpolicy.ManifestPath(targetDir), 0444); err != nil {
		h.failIfTerminal(ctx, job, snap.ID, "SNAPSHOT_SEAL_FAILED")
		return jobs.NewRetryableError("SNAPSHOT_SEAL_FAILED", err.Error(), err)
	}
	stageFinalized := false
	if h.finalizer != nil && job.WorkerID != nil && job.ClaimToken != nil && snap.AnalysisRevisionID != "" {
		if err := h.finalizer.FinalizeSnapshot(ctx, analysispipeline.SnapshotStageResult{
			Ownership:  analysispipeline.JobOwnership{JobID: job.ID, WorkerID: *job.WorkerID, ClaimToken: *job.ClaimToken},
			RevisionID: snap.AnalysisRevisionID, SnapshotID: snap.ID, MaterializedPath: targetDir,
			ModulePath: r.Name, CommitSHA: commitSHA, ContentHash: contentHash,
			FileCount: fileCount, TotalBytes: totalBytes, ReadyAt: now,
		}); err != nil {
			return jobs.WrapAtomicHandlerFinalization(jobs.StatusSucceeded, err)
		}
		stageFinalized = true
	} else if finalizer, ok := h.snapshotStore.(snapshot.ClaimedMaterializationFinalizer); ok && job.WorkerID != nil && job.ClaimToken != nil {
		if err := finalizer.FinalizeSnapshotSuccess(ctx, job.ID, *job.WorkerID, *job.ClaimToken, snap.ID, targetDir, commitSHA, contentHash, fileCount, totalBytes, now); err != nil {
			return jobs.WrapAtomicHandlerFinalization(jobs.StatusSucceeded, err)
		}
	} else if finalizer, ok := h.snapshotStore.(snapshot.MaterializationFinalizer); ok {
		if err := finalizer.FinalizeMaterialization(ctx, snap.ID, targetDir, commitSHA, contentHash, fileCount, totalBytes, now); err != nil {
			h.failIfTerminal(ctx, job, snap.ID, "SNAPSHOT_FINALIZE_FAILED")
			return jobs.NewRetryableError("SNAPSHOT_FINALIZE_FAILED", err.Error(), err)
		}
	} else if err := h.snapshotStore.UpdateStatus(ctx, snap.ID, snapshot.StatusMaterializing, snapshot.StatusReady, &now); err != nil {
		return jobs.NewRetryableError("SNAPSHOT_FINALIZE_FAILED", err.Error(), err)
	}
	if h.finalizer == nil && !stageFinalized && h.revisionStore != nil && snap.AnalysisRevisionID != "" {
		if err := h.revisionStore.MarkSnapshotReady(ctx, snap.AnalysisRevisionID, snap.ID); err != nil {
			return jobs.NewRetryableError("REVISION_STAGE_UPDATE_FAILED", err.Error(), err)
		}
	}

	// Auto-chain BUILD_CODE_INDEX job if codeIntelStore is wired
	if h.codeIntelStore != nil && !stageFinalized && snap.AnalysisRevisionID == "" {
		if err := h.createLegacyCodeIndexHandoff(ctx, snap); err != nil {
			return err
		}
	}

	log.Info("snapshot materialization completed successfully", "chunks", len(allChunks), "docs", docCount)
	return nil
}

func (h *SnapshotJobHandler) createLegacyCodeIndexHandoff(ctx context.Context, snap *snapshot.RepositorySnapshot) error {
	repository, err := h.repoStore.GetByID(ctx, snap.RepositoryID)
	if err != nil {
		return jobs.NewRetryableError("CODE_INDEX_HANDOFF_FAILED", "failed to load repository for automatic code index handoff", err)
	}
	if _, _, err := h.codeIntelStore.GetOrCreateBuild(ctx, snap.ID, repository.Name, codeintelmodel.DefaultBuildContext()); err != nil {
		return jobs.NewRetryableError("CODE_INDEX_HANDOFF_FAILED", "snapshot is READY but automatic code index build creation failed", err)
	}
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
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
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

func jobExecutionClaimToken(job *jobs.AnalysisJob) string {
	if job != nil && job.ClaimToken != nil && *job.ClaimToken != "" {
		return *job.ClaimToken
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}
