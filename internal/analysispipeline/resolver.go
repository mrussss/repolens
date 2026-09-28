package analysispipeline

import (
	"context"
	"errors"
	"fmt"

	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

// Resolver is the READY revision lineage validation boundary.
type Resolver struct {
	revisions revision.Store
	snapshots snapshot.Store
	codeintel codeintelstore.Store
}

func NewResolver(revisions revision.Store, snapshots snapshot.Store, codeintel codeintelstore.Store) *Resolver {
	return &Resolver{revisions: revisions, snapshots: snapshots, codeintel: codeintel}
}

// RepositoryForRevision exposes only the repository identity needed to
// authorize a revision before revealing the state of its derived artifacts.
func (r *Resolver) RepositoryForRevision(ctx context.Context, revisionID string) (string, error) {
	if r == nil || r.revisions == nil {
		return "", errors.New("READY lineage resolver is not configured")
	}
	rev, err := r.revisions.GetByID(ctx, revisionID)
	if err != nil {
		return "", err
	}
	if rev == nil {
		return "", revision.ErrNotFound
	}
	return rev.RepositoryID, nil
}

func (r *Resolver) ResolveLegacyReady(ctx context.Context, repositoryID, snapshotID string, codeIndexBuildID, retrievalBuildID int64) (ResolvedLineage, error) {
	if r == nil || r.snapshots == nil {
		return ResolvedLineage{}, errors.New("READY lineage resolver is not configured")
	}
	if repositoryID == "" || snapshotID == "" || codeIndexBuildID <= 0 || retrievalBuildID <= 0 {
		return ResolvedLineage{}, codeintelstore.ErrBuildLineageMismatch
	}
	snap, err := r.snapshots.GetByID(ctx, snapshotID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ResolvedLineage{}, fmt.Errorf("snapshot %s not found", snapshotID)
		}
		return ResolvedLineage{}, err
	}
	if snap == nil {
		return ResolvedLineage{}, fmt.Errorf("snapshot %s not found", snapshotID)
	}
	if snap.Status != snapshot.StatusReady {
		return ResolvedLineage{}, fmt.Errorf("snapshot %s is not READY (current status: %s)", snapshotID, snap.Status)
	}
	if r.codeintel == nil {
		return ResolvedLineage{RepositoryID: repositoryID, CommitSHA: snap.CommitSHA, SnapshotID: snap.ID, CodeIndexBuildID: codeIndexBuildID, RetrievalBuildID: retrievalBuildID}, nil
	}
	if snap.RepositoryID != repositoryID {
		return ResolvedLineage{}, codeintelstore.ErrBuildLineageMismatch
	}
	if err := r.codeintel.ValidateLineage(ctx, repositoryID, snapshotID, codeIndexBuildID, retrievalBuildID); err != nil {
		return ResolvedLineage{}, err
	}
	codeBuild, err := r.codeintel.GetByID(ctx, codeIndexBuildID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ResolvedLineage{}, fmt.Errorf("%w: code index build %d", ErrBuildNotReady, codeIndexBuildID)
		}
		return ResolvedLineage{}, fmt.Errorf("failed to load code index build %d: %w", codeIndexBuildID, err)
	}
	if codeBuild == nil {
		return ResolvedLineage{}, fmt.Errorf("%w: code index build %d", ErrBuildNotReady, codeIndexBuildID)
	}
	if codeBuild.Status != codeintelmodel.BuildStatusReady {
		return ResolvedLineage{}, fmt.Errorf("%w: code index build %d is %s", ErrBuildNotReady, codeIndexBuildID, codeBuild.Status)
	}
	retrievalBuild, err := r.codeintel.GetRetrievalBuildByID(ctx, retrievalBuildID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ResolvedLineage{}, fmt.Errorf("%w: retrieval build %d", ErrBuildNotReady, retrievalBuildID)
		}
		return ResolvedLineage{}, fmt.Errorf("failed to load retrieval build %d: %w", retrievalBuildID, err)
	}
	if retrievalBuild == nil {
		return ResolvedLineage{}, fmt.Errorf("%w: retrieval build %d", ErrBuildNotReady, retrievalBuildID)
	}
	if retrievalBuild.Status != codeintelmodel.BuildStatusReady {
		return ResolvedLineage{}, fmt.Errorf("%w: retrieval build %d is %s", ErrBuildNotReady, retrievalBuildID, retrievalBuild.Status)
	}
	return ResolvedLineage{
		RepositoryID:     repositoryID,
		CommitSHA:        snap.CommitSHA,
		SnapshotID:       snap.ID,
		CodeIndexBuildID: codeBuild.ID,
		RetrievalBuildID: retrievalBuild.ID,
	}, nil
}

func (r *Resolver) ResolveReadyLineage(ctx context.Context, revisionID string) (ResolvedLineage, error) {
	if r == nil || r.revisions == nil || r.snapshots == nil || r.codeintel == nil {
		return ResolvedLineage{}, errors.New("READY lineage resolver is not configured")
	}

	rev, err := r.revisions.GetByID(ctx, revisionID)
	if err != nil {
		return ResolvedLineage{}, err
	}
	if rev == nil {
		return ResolvedLineage{}, revision.ErrNotFound
	}
	if rev.Status != revision.StatusReady || rev.Stage != revision.StageReady {
		return ResolvedLineage{}, ErrRevisionNotReady
	}
	if rev.ID == "" || rev.RepositoryID == "" || rev.CommitSHA == "" || rev.SnapshotID == "" || rev.CodeIndexBuildID <= 0 || rev.RetrievalBuildID <= 0 || rev.PipelineVersion == "" || rev.PipelineFingerprint == "" {
		return ResolvedLineage{}, fmt.Errorf("%w: revision %s has incomplete READY identity", codeintelstore.ErrBuildLineageMismatch, rev.ID)
	}

	snap, err := r.snapshots.GetByID(ctx, rev.SnapshotID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ResolvedLineage{}, fmt.Errorf("snapshot %s not found", rev.SnapshotID)
		}
		return ResolvedLineage{}, err
	}
	if snap == nil {
		return ResolvedLineage{}, fmt.Errorf("snapshot %s not found", rev.SnapshotID)
	}
	if snap.Status != snapshot.StatusReady {
		return ResolvedLineage{}, fmt.Errorf("snapshot %s is not READY (current status: %s)", rev.SnapshotID, snap.Status)
	}
	if snap.RepositoryID != rev.RepositoryID || snap.AnalysisRevisionID != rev.ID || snap.CommitSHA != rev.CommitSHA {
		return ResolvedLineage{}, codeintelstore.ErrBuildLineageMismatch
	}

	codeBuild, err := r.codeintel.GetByID(ctx, rev.CodeIndexBuildID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ResolvedLineage{}, fmt.Errorf("%w: code index build %d", ErrBuildNotReady, rev.CodeIndexBuildID)
		}
		return ResolvedLineage{}, fmt.Errorf("failed to load code index build %d: %w", rev.CodeIndexBuildID, err)
	}
	if codeBuild == nil {
		return ResolvedLineage{}, fmt.Errorf("%w: code index build %d", ErrBuildNotReady, rev.CodeIndexBuildID)
	}
	if codeBuild.SnapshotID != snap.ID || codeBuild.AnalysisRevisionID != rev.ID {
		return ResolvedLineage{}, codeintelstore.ErrBuildLineageMismatch
	}
	if codeBuild.Status != codeintelmodel.BuildStatusReady {
		return ResolvedLineage{}, fmt.Errorf("%w: code index build %d is %s", ErrBuildNotReady, codeBuild.ID, codeBuild.Status)
	}

	retrievalBuild, err := r.codeintel.GetRetrievalBuildByID(ctx, rev.RetrievalBuildID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ResolvedLineage{}, fmt.Errorf("%w: retrieval build %d", ErrBuildNotReady, rev.RetrievalBuildID)
		}
		return ResolvedLineage{}, fmt.Errorf("failed to load retrieval build %d: %w", rev.RetrievalBuildID, err)
	}
	if retrievalBuild == nil {
		return ResolvedLineage{}, fmt.Errorf("%w: retrieval build %d", ErrBuildNotReady, rev.RetrievalBuildID)
	}
	if retrievalBuild.CodeIndexBuildID != codeBuild.ID || retrievalBuild.AnalysisRevisionID != rev.ID {
		return ResolvedLineage{}, codeintelstore.ErrBuildLineageMismatch
	}
	if retrievalBuild.Status != codeintelmodel.BuildStatusReady {
		return ResolvedLineage{}, fmt.Errorf("%w: retrieval build %d is %s", ErrBuildNotReady, retrievalBuild.ID, retrievalBuild.Status)
	}

	if err := r.codeintel.ValidateLineage(ctx, rev.RepositoryID, snap.ID, codeBuild.ID, retrievalBuild.ID); err != nil {
		return ResolvedLineage{}, err
	}
	return ResolvedLineage{
		RepositoryID:        rev.RepositoryID,
		RevisionID:          rev.ID,
		CommitSHA:           rev.CommitSHA,
		SnapshotID:          snap.ID,
		CodeIndexBuildID:    codeBuild.ID,
		RetrievalBuildID:    retrievalBuild.ID,
		PipelineVersion:     rev.PipelineVersion,
		PipelineFingerprint: rev.PipelineFingerprint,
	}, nil
}
