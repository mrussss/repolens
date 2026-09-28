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
