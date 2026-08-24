package repo

import (
	"time"
)

type RepositoryStatus string

const (
	StatusActive   RepositoryStatus = "ACTIVE"
	StatusDisabled RepositoryStatus = "DISABLED"
	StatusDeleted  RepositoryStatus = "DELETED"
)

type Repository struct {
	ID         string           `gorm:"primaryKey;size:36" json:"id"`
	UserID     string           `gorm:"index;size:36;not null" json:"user_id"`
	Name       string           `gorm:"size:128;not null" json:"name"`
	GitURL     string           `gorm:"size:512;not null" json:"git_url"`
	DefaultRef string           `gorm:"size:128;not null;default:'main'" json:"default_ref"`
	Status     RepositoryStatus `gorm:"size:32;not null;default:'ACTIVE'" json:"status"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}
