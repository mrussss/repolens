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

// MaterializationFinalizer is implemented by the SQL-backed store.  It keeps
// the identity fields and READY transition in one conditional update so a
// partially materialized directory can never be advertised as a snapshot.
type MaterializationFinalizer interface {
	FinalizeMaterialization(ctx context.Context, id, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error
	FailMaterialization(ctx context.Context, id, errorCode string) error
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
	if expectedOldStatus == StatusReady || (expectedOldStatus != StatusCreated && expectedOldStatus != StatusMaterializing) {
		return fmt.Errorf("snapshot %s has immutable or invalid source state %s", id, expectedOldStatus)
	}
	if expectedOldStatus == StatusCreated && newStatus != StatusMaterializing || expectedOldStatus == StatusMaterializing && newStatus != StatusReady && newStatus != StatusFailed {
		return fmt.Errorf("invalid snapshot transition %s -> %s", expectedOldStatus, newStatus)
	}
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

func (s *GormStore) FinalizeMaterialization(ctx context.Context, id, commitSHA, contentHash string, fileCount int, totalBytes int64, readyAt time.Time) error {
	if commitSHA == "" || commitSHA == "pending" || contentHash == "" {
		return fmt.Errorf("snapshot %s cannot become READY without exact commit and content hash", id)
	}
	result := s.db.WithContext(ctx).Model(&RepositorySnapshot{}).
		Where("id = ? AND status = ?", id, StatusMaterializing).
		Updates(map[string]interface{}{
			"commit_sha":   commitSHA,
			"content_hash": contentHash,
			"file_count":   fileCount,
			"total_bytes":  totalBytes,
			"status":       StatusReady,
			"ready_at":     readyAt,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("snapshot %s materialization finalize conflict", id)
	}
	return nil
}

func (s *GormStore) FailMaterialization(ctx context.Context, id, errorCode string) error {
	result := s.db.WithContext(ctx).Model(&RepositorySnapshot{}).
		Where("id = ? AND status = ?", id, StatusMaterializing).
		Updates(map[string]interface{}{"status": StatusFailed, "error_code": errorCode})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("snapshot %s failure transition conflict", id)
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
