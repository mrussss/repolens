package repo

import (
	"context"
	"path/filepath"
	"repolens/internal/snapshot"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGormStoreListByUserStatus(t *testing.T) {
	// 1. 创建临时 SQLite 路径
	dbPath := filepath.Join(
		t.TempDir(),
		"repo_test.db",
	)
	// 2. gorm.Open
	db, err := gorm.Open(
		sqlite.Open(dbPath),
		&gorm.Config{},
	)
	// 3. 检查 err
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	// 4. AutoMigrate Repository + RepositorySnapshot
	err = db.AutoMigrate(
		&Repository{},
		&snapshot.RepositorySnapshot{},
	)
	// 5. 检查 err
	if err != nil {
		t.Fatalf("failed to migrate test database: %v", err)
	}

	repos := []Repository{
		{
			ID:         "repo-1",
			UserID:     "user-1",
			Name:       "active-1",
			GitURL:     "https://example.com/repo-1.git",
			DefaultRef: "main",
			Status:     StatusActive,
		},
		{
			ID:         "repo-2",
			UserID:     "user-1",
			Name:       "active-2",
			GitURL:     "https://example.com/repo-2.git",
			DefaultRef: "main",
			Status:     StatusActive,
		},
		{
			ID:         "repo-3",
			UserID:     "user-1",
			Name:       "disabled-1",
			GitURL:     "https://example.com/repo-3.git",
			DefaultRef: "main",
			Status:     StatusDisabled,
		},
		{
			ID:         "repo-4",
			UserID:     "user-1",
			Name:       "deleted-1",
			GitURL:     "https://example.com/repo-4.git",
			DefaultRef: "main",
			Status:     StatusDeleted,
		},
		{
			ID:         "repo-5",
			UserID:     "user-2",
			Name:       "other-user-active",
			GitURL:     "https://example.com/repo-5.git",
			DefaultRef: "main",
			Status:     StatusActive,
		},
	}

	err = db.Create(&repos).Error
	if err != nil {
		t.Fatalf(
			"failed to create test repositories: %v",
			err,
		)
	}

	store := NewStore(db)
	got, total, err := store.ListByUser(
		context.Background(),
		"user-1",
		1,
		20,
		string(StatusActive),
	)
	if err != nil {
		t.Fatalf(
			"ListByUser() error = %v",
			err,
		)
	}
	if total != 2 {
		t.Fatalf(
			"expected total 2, got %d",
			total,
		)
	}
	if len(got) != 2 {
		t.Fatalf(
			"expected 2 repositories, got %d",
			len(got),
		)
	}
	for _, r := range got {
		if r.Status != StatusActive {
			t.Fatalf(
				"expected status %q, got %q",
				StatusActive,
				r.Status,
			)
		}
	}
	got, total, err = store.ListByUser(
		context.Background(),
		"user-1",
		1,
		20,
		string(StatusDisabled),
	)
	if err != nil {
		t.Fatalf(
			"ListByUser() error = %v",
			err,
		)
	}
	if total != 1 {
		t.Fatalf(
			"expected total 1, got %d",
			total,
		)
	}
	if len(got) != 1 {
		t.Fatalf(
			"expected 1 repositories, got %d",
			len(got),
		)
	}

	if got[0].Status != StatusDisabled {
		t.Fatalf(
			"expected status %q, got %q",
			StatusDisabled,
			got[0].Status,
		)
	}
	got, total, err = store.ListByUser(
		context.Background(),
		"user-1",
		1,
		20,
		"",
	)
	if err != nil {
		t.Fatalf(
			"ListByUser() error = %v",
			err,
		)
	}
	if total != 3 {
		t.Fatalf(
			"expected total 3, got %d",
			total,
		)
	}
	if len(got) != 3 {
		t.Fatalf(
			"expected 3 repositories, got %d",
			len(got),
		)
	}
	for _, r := range got {
		if r.UserID != "user-1" {
			t.Fatalf(
				"expected user_id %q, got %q",
				"user-1",
				r.UserID,
			)
		}

		if r.Status == StatusDeleted {
			t.Fatalf(
				"expected deleted repository to be excluded",
			)
		}
	}
}
