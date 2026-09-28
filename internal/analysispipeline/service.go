package analysispipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"repolens/internal/repo"
	"repolens/internal/revision"
)

type RefResolver interface {
	ResolveRef(ctx context.Context, gitURL, ref string) (string, error)
}

// Service owns preparation and retry transactions.
type Service struct {
	store *Store
}

func NewService(store *Store) *Service { return &Service{store: store} }

func (s *Service) Prepare(ctx context.Context, spec PrepareSpec) (*revision.AnalysisRevision, error) {
	return s.store.CreatePreparation(ctx, spec)
}

func (s *Service) Retry(ctx context.Context, revisionID string) (*revision.AnalysisRevision, error) {
	return s.store.RetryPreparation(ctx, revisionID)
}

// RevisionService keeps the existing revision HTTP use cases while delegating
// preparation and retry writes to the AnalysisPipeline service.
type RevisionService struct {
	*revision.Service
	pipeline  *Service
	revisions revision.Store
	repoStore repo.Store
	resolver  RefResolver
	basePath  string
}

func NewRevisionService(pipeline *Service, revisions revision.Store, repoStore repo.Store, resolver RefResolver, snapshotBasePath string) *RevisionService {
	return &RevisionService{Service: revision.NewService(revisions, repoStore), pipeline: pipeline, revisions: revisions, repoStore: repoStore, resolver: resolver, basePath: snapshotBasePath}
}

func (s *RevisionService) Prepare(ctx context.Context, userID, repositoryID, requestedRef string) (*revision.AnalysisRevision, bool, error) {
	if strings.TrimSpace(requestedRef) == "" {
		r, err := s.repoStore.GetByIDAndUser(ctx, repositoryID, userID)
		if err != nil {
			return nil, false, err
		}
		requestedRef = r.DefaultRef
	}
	if err := repo.ValidateRefLength(requestedRef); err != nil {
		return nil, false, err
	}
	repository, err := s.repoStore.GetByIDAndUser(ctx, repositoryID, userID)
	if err != nil {
		return nil, false, err
	}
	if s.resolver == nil {
		return nil, false, fmt.Errorf("revision ref resolver is not configured")
	}
	commitSHA, err := s.resolver.ResolveRef(ctx, repository.GitURL, requestedRef)
	if err != nil {
		return nil, false, fmt.Errorf("%w: %v", revision.ErrRefResolution, err)
	}
	fingerprint := revision.ComputePipelineFingerprint()
	existing, lookupErr := s.revisions.GetByIdentity(ctx, repositoryID, commitSHA, fingerprint)
	if lookupErr == nil {
		if existing.Status == revision.StatusFailed {
			return existing, false, revision.ErrFailedRevision
		}
		return existing, false, nil
	}
	if !errors.Is(lookupErr, revision.ErrNotFound) {
		return nil, false, lookupErr
	}

	created, err := s.pipeline.Prepare(ctx, PrepareSpec{
		RepositoryID: repositoryID, SourceRef: requestedRef, CommitSHA: commitSHA,
		PipelineVersion: revision.PipelineVersion, PipelineFingerprint: fingerprint,
		SnapshotBasePath: s.basePath, ModulePath: repository.Name,
	})
	if err != nil {
		// The unique identity is the concurrency boundary. A losing request
		// re-reads the winner rather than exposing a database duplicate error.
		if winner, winnerErr := s.revisions.GetByIdentity(ctx, repositoryID, commitSHA, fingerprint); winnerErr == nil {
			if winner.Status == revision.StatusFailed {
				return winner, false, revision.ErrFailedRevision
			}
			return winner, false, nil
		}
		return nil, false, err
	}
	return created, true, nil
}

func (s *RevisionService) Retry(ctx context.Context, userID, id string) (*revision.AnalysisRevision, error) {
	value, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if value.Status != revision.StatusFailed {
		if value.Status == revision.StatusPreparing {
			return nil, revision.ErrRetryConflict
		}
		return nil, revision.ErrInvalidState
	}
	return s.pipeline.Retry(ctx, id)
}
