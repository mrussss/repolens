package revision

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"repolens/internal/repo"
)

type RefResolver interface {
	ResolveRef(ctx context.Context, gitURL, ref string) (string, error)
}

type Service struct {
	store     Store
	repoStore repo.Store
	resolver  RefResolver
	basePath  string
}

func NewService(store Store, repoStore repo.Store, resolver RefResolver, snapshotBasePath string) *Service {
	return &Service{store: store, repoStore: repoStore, resolver: resolver, basePath: snapshotBasePath}
}

func (s *Service) Prepare(ctx context.Context, userID, repositoryID, requestedRef string) (*AnalysisRevision, bool, error) {
	if strings.TrimSpace(requestedRef) == "" {
		r, err := s.repoStore.GetByIDAndUser(ctx, repositoryID, userID)
		if err != nil {
			return nil, false, err
		}
		requestedRef = r.DefaultRef
	}
	if len(requestedRef) > 255 {
		return nil, false, fmt.Errorf("ref exceeds maximum length")
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
		return nil, false, fmt.Errorf("ref resolution failed: %w", err)
	}
	fingerprint := ComputePipelineFingerprint()
	existing, lookupErr := s.store.GetByIdentity(ctx, repositoryID, commitSHA, fingerprint)
	if lookupErr == nil {
		if existing.Status == StatusFailed {
			return existing, false, ErrFailedRevision
		}
		return existing, false, nil
	}
	if !errors.Is(lookupErr, ErrNotFound) {
		return nil, false, lookupErr
	}

	created, err := s.store.CreatePreparation(ctx, PrepareSpec{
		RepositoryID:        repositoryID,
		SourceRef:           requestedRef,
		CommitSHA:           commitSHA,
		PipelineVersion:     PipelineVersion,
		PipelineFingerprint: fingerprint,
		SnapshotBasePath:    s.basePath,
		ModulePath:          repository.Name,
	})
	if err != nil {
		// The unique identity is the concurrency boundary. A losing request
		// re-reads the winner rather than exposing a database duplicate error.
		if winner, winnerErr := s.store.GetByIdentity(ctx, repositoryID, commitSHA, fingerprint); winnerErr == nil {
			if winner.Status == StatusFailed {
				return winner, false, ErrFailedRevision
			}
			return winner, false, nil
		}
		return nil, false, err
	}
	return created, true, nil
}

func (s *Service) Get(ctx context.Context, userID, id string) (*AnalysisRevision, error) {
	value, err := s.store.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := s.repoStore.GetByIDAndUser(ctx, value.RepositoryID, userID); err != nil {
		return nil, ErrNotFound
	}
	return value, nil
}

func (s *Service) List(ctx context.Context, userID, repositoryID string, limit int) ([]AnalysisRevision, error) {
	if _, err := s.repoStore.GetByIDAndUser(ctx, repositoryID, userID); err != nil {
		return nil, ErrNotFound
	}
	return s.store.ListByRepository(ctx, repositoryID, limit)
}

func (s *Service) Retry(ctx context.Context, userID, id string) (*AnalysisRevision, error) {
	value, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	if value.Status != StatusFailed {
		return nil, ErrInvalidState
	}
	return s.store.Retry(ctx, id)
}
