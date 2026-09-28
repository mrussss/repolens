package revision

import (
	"context"

	"repolens/internal/repo"
)

// Service retains the revision read side; AnalysisPipeline owns preparation
// and retry writes.
type Service struct {
	store     Store
	repoStore repo.Store
}

func NewService(store Store, repoStore repo.Store) *Service {
	return &Service{store: store, repoStore: repoStore}
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
