package snapshot

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Store interface {
	Create(ctx context.Context, s *RepositorySnapshot) error
	GetByID(ctx context.Context, id string) (*RepositorySnapshot, error)
	GetLatestReady(ctx context.Context, repoID string) (*RepositorySnapshot, error)
	GetByCommit(ctx context.Context, repoID, commitSHA string) (*RepositorySnapshot, error)
	UpdateStatus(ctx context.Context, id string, expectedOldStatus, newStatus SnapshotStatus, readyAt *time.Time) error
}

type GormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) Create(ctx context.Context, snap *RepositorySnapshot) error {
	if snap.ID == "" {
		snap.ID = uuid.New().String()
	}
	return s.db.WithContext(ctx).Create(snap).Error
}

func (s *GormStore) GetByID(ctx context.Context, id string) (*RepositorySnapshot, error) {
	var snap RepositorySnapshot
	if err := s.db.WithContext(ctx).First(&snap, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *GormStore) GetLatestReady(ctx context.Context, repoID string) (*RepositorySnapshot, error) {
	var snap RepositorySnapshot
	if err := s.db.WithContext(ctx).Where("repository_id = ? AND status = ?", repoID, StatusReady).Order("created_at DESC").First(&snap).Error; err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *GormStore) GetByCommit(ctx context.Context, repoID, commitSHA string) (*RepositorySnapshot, error) {
	var snap RepositorySnapshot
	if err := s.db.WithContext(ctx).Where("repository_id = ? AND commit_sha = ?", repoID, commitSHA).First(&snap).Error; err != nil {
		return nil, err
	}
	return &snap, nil
}

func (s *GormStore) UpdateStatus(ctx context.Context, id string, expectedOldStatus, newStatus SnapshotStatus, readyAt *time.Time) error {
	updates := map[string]interface{}{
		"status": newStatus,
	}
	if readyAt != nil {
		updates["ready_at"] = readyAt
	}

	result := s.db.WithContext(ctx).Model(&RepositorySnapshot{}).
		Where("id = ? AND status = ?", id, expectedOldStatus).
		Updates(updates)

	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return fmt.Errorf("snapshot %s status transition conflict: expected %s", id, expectedOldStatus)
	}
	return nil
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Create(ctx context.Context, repoID, commitSHA, ref, materializedPath string) (*RepositorySnapshot, error) {
	snap := &RepositorySnapshot{
		ID:               uuid.New().String(),
		RepositoryID:     repoID,
		CommitSHA:        commitSHA,
		Ref:              ref,
		MaterializedPath: materializedPath,
		Status:           StatusCreated,
	}
	if err := s.store.Create(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func (s *Service) Get(ctx context.Context, id string) (*RepositorySnapshot, error) {
	snap, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("snapshot not found")
		}
		return nil, err
	}
	return snap, nil
}
