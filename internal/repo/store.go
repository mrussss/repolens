package repo

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"gorm.io/gorm"

	"repolens/internal/snapshot"
)

type Store interface {
	Create(ctx context.Context, r *Repository) error
	GetByID(ctx context.Context, id string) (*Repository, error)
	GetByIDAndUser(ctx context.Context, id, userID string) (*Repository, error)
	ListByUser(ctx context.Context, userID string, page, pageSize int) ([]Repository, int64, error)
	Update(ctx context.Context, r *Repository) error
}

type GormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) Create(ctx context.Context, r *Repository) error {
	if r.ID == "" {
		r.ID = uuid.New().String()
	}
	return s.db.WithContext(ctx).Create(r).Error
}

func (s *GormStore) GetByID(ctx context.Context, id string) (*Repository, error) {
	var r Repository
	if err := s.db.WithContext(ctx).First(&r, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *GormStore) GetByIDAndUser(ctx context.Context, id, userID string) (*Repository, error) {
	var r Repository
	if err := s.db.WithContext(ctx).First(&r, "id = ? AND user_id = ?", id, userID).Error; err != nil {
		return nil, err
	}
	return &r, nil
}

func (s *GormStore) ListByUser(ctx context.Context, userID string, page, pageSize int) ([]Repository, int64, error) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	offset := (page - 1) * pageSize

	var repos []Repository
	var total int64

	tx := s.db.WithContext(ctx).Model(&Repository{}).Where("user_id = ? AND status != ?", userID, StatusDeleted)
	if err := tx.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	if err := tx.Preload("Snapshots", func(db *gorm.DB) *gorm.DB {
		return db.Where("status != ?", snapshot.StatusFailed).Order("created_at DESC")
	}).Order("created_at DESC").Offset(offset).Limit(pageSize).Find(&repos).Error; err != nil {
		return nil, 0, err
	}

	return repos, total, nil
}

func (s *GormStore) Update(ctx context.Context, r *Repository) error {
	return s.db.WithContext(ctx).Save(r).Error
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Register(ctx context.Context, userID, name, gitURL, defaultRef string) (*Repository, error) {
	if defaultRef == "" {
		defaultRef = "main"
	}
	r := &Repository{
		ID:         uuid.New().String(),
		UserID:     userID,
		Name:       name,
		GitURL:     gitURL,
		DefaultRef: defaultRef,
		Status:     StatusActive,
	}
	if err := s.store.Create(ctx, r); err != nil {
		return nil, fmt.Errorf("failed to register repository: %w", err)
	}
	return r, nil
}

func (s *Service) Get(ctx context.Context, id, userID string) (*Repository, error) {
	r, err := s.store.GetByIDAndUser(ctx, id, userID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.New("repository not found")
		}
		return nil, err
	}
	return r, nil
}

func (s *Service) List(ctx context.Context, userID string, page, pageSize int) ([]Repository, int64, error) {
	return s.store.ListByUser(ctx, userID, page, pageSize)
}
