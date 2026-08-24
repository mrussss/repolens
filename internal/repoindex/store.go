package repoindex

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Store interface {
	Create(ctx context.Context, idx *RepositoryIndex) error
	GetByID(ctx context.Context, id string) (*RepositoryIndex, error)
	GetBySnapshotAndStrategy(ctx context.Context, snapshotID string, strategy RetrievalStrategy) (*RepositoryIndex, error)
	GetReadyIndex(ctx context.Context, snapshotID string, strategy RetrievalStrategy) (*RepositoryIndex, error)
	UpdateStatus(ctx context.Context, id string, expectedOldStatus, newStatus IndexStatus, readyAt *time.Time, chunkCount, docCount int, errCode string) error
}

type GormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) Create(ctx context.Context, idx *RepositoryIndex) error {
	if idx.ID == "" {
		idx.ID = uuid.New().String()
	}
	return s.db.WithContext(ctx).Create(idx).Error
}

func (s *GormStore) GetByID(ctx context.Context, id string) (*RepositoryIndex, error) {
	var idx RepositoryIndex
	if err := s.db.WithContext(ctx).First(&idx, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &idx, nil
}

func (s *GormStore) GetBySnapshotAndStrategy(ctx context.Context, snapshotID string, strategy RetrievalStrategy) (*RepositoryIndex, error) {
	var idx RepositoryIndex
	if err := s.db.WithContext(ctx).Where("snapshot_id = ? AND strategy = ?", snapshotID, strategy).First(&idx).Error; err != nil {
		return nil, err
	}
	return &idx, nil
}

func (s *GormStore) GetReadyIndex(ctx context.Context, snapshotID string, strategy RetrievalStrategy) (*RepositoryIndex, error) {
	var idx RepositoryIndex
	if err := s.db.WithContext(ctx).Where("snapshot_id = ? AND strategy = ? AND status = ?", snapshotID, strategy, StatusReady).First(&idx).Error; err != nil {
		return nil, err
	}
	return &idx, nil
}

func (s *GormStore) UpdateStatus(ctx context.Context, id string, expectedOldStatus, newStatus IndexStatus, readyAt *time.Time, chunkCount, docCount int, errCode string) error {
	updates := map[string]interface{}{
		"status": newStatus,
	}
	if readyAt != nil {
		updates["ready_at"] = readyAt
	}
	if chunkCount > 0 {
		updates["chunk_count"] = chunkCount
	}
	if docCount > 0 {
		updates["document_count"] = docCount
	}
	if errCode != "" {
		updates["error_code"] = errCode
	}

	result := s.db.WithContext(ctx).Model(&RepositoryIndex{}).
		Where("id = ? AND status = ?", id, expectedOldStatus).
		Updates(updates)

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("repoindex %s status transition conflict: expected %s", id, expectedOldStatus)
	}
	return nil
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Get(ctx context.Context, id string) (*RepositoryIndex, error) {
	idx, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("repository index not found")
		}
		return nil, err
	}
	return idx, nil
}
