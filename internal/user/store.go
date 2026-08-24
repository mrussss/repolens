package user

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

type Store interface {
	Create(ctx context.Context, u *User) error
	GetByID(ctx context.Context, id string) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
}

type GormStore struct {
	db *gorm.DB
}

func NewStore(db *gorm.DB) *GormStore {
	return &GormStore{db: db}
}

func (s *GormStore) Create(ctx context.Context, u *User) error {
	if u.ID == "" {
		u.ID = uuid.New().String()
	}
	return s.db.WithContext(ctx).Create(u).Error
}

func (s *GormStore) GetByID(ctx context.Context, id string) (*User, error) {
	var u User
	if err := s.db.WithContext(ctx).First(&u, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *GormStore) GetByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	if err := s.db.WithContext(ctx).First(&u, "email = ?", email).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

type Service struct {
	store Store
}

func NewService(store Store) *Service {
	return &Service{store: store}
}

func (s *Service) Register(ctx context.Context, email, password string) (*User, error) {
	existing, err := s.store.GetByEmail(ctx, email)
	if err == nil && existing != nil {
		return nil, errors.New("email already registered")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("failed to hash password: %w", err)
	}

	u := &User{
		ID:           uuid.New().String(),
		Email:        email,
		PasswordHash: string(hash),
	}
	if err := s.store.Create(ctx, u); err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}
	return u, nil
}

func (s *Service) Authenticate(ctx context.Context, email, password string) (*User, error) {
	u, err := s.store.GetByEmail(ctx, email)
	if err != nil {
		return nil, errors.New("invalid email or password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(password)); err != nil {
		return nil, errors.New("invalid email or password")
	}
	return u, nil
}
