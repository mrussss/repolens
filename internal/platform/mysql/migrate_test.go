package mysql

import (
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/snapshot"
)

// legacyRepositorySnapshot models the SQLite schema before analysis-revision
// scoped snapshot identities: its revision ID was nullable and repo+commit was
// the only uniqueness boundary.
type legacyRepositorySnapshot struct {
	ID                 string     `gorm:"primaryKey;size:64"`
	RepositoryID       string     `gorm:"size:64;not null;uniqueIndex:uq_repo_commit,priority:1;index:ix_repo_snap"`
	AnalysisRevisionID *string    `gorm:"size:36;index"`
	CommitSHA          string     `gorm:"size:64;not null;uniqueIndex:uq_repo_commit,priority:2"`
	Ref                string     `gorm:"size:128;not null"`
	RequestedRef       string     `gorm:"size:128"`
	MaterializedPath   string     `gorm:"size:512;not null"`
	ContentHash        string     `gorm:"size:64;not null"`
	Status             string     `gorm:"size:32;not null;default:'CREATED'"`
	FileCount          int        `gorm:"not null;default:0"`
	TotalBytes         int64      `gorm:"not null;default:0"`
	ErrorCode          string     `gorm:"size:64"`
	CreatedAt          time.Time  `json:"created_at"`
	ReadyAt            *time.Time `json:"ready_at,omitempty"`
}

func (legacyRepositorySnapshot) TableName() string { return "repository_snapshots" }

func TestAutoMigrateUpgradesLegacyNullableSnapshotRevisionID(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "legacy.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&legacyRepositorySnapshot{}); err != nil {
		t.Fatalf("create legacy sqlite schema: %v", err)
	}
	legacy := &legacyRepositorySnapshot{
		ID: "legacy-snapshot", RepositoryID: "repo-upgrade", CommitSHA: "same-commit",
		Ref: "main", MaterializedPath: "/tmp/legacy-source", ContentHash: "legacy-hash", Status: "READY",
	}
	if err := db.Create(legacy).Error; err != nil {
		t.Fatalf("seed legacy snapshot with NULL revision ID: %v", err)
	}

	if err := AutoMigrate(db); err != nil {
		t.Fatalf("upgrade legacy sqlite schema: %v", err)
	}
	var upgraded snapshot.RepositorySnapshot
	if err := db.First(&upgraded, "id = ?", legacy.ID).Error; err != nil {
		t.Fatalf("read upgraded legacy snapshot: %v", err)
	}
	if upgraded.AnalysisRevisionID != "" || upgraded.Status != snapshot.StatusReady {
		t.Fatalf("upgraded legacy snapshot = %+v, want empty revision ID and preserved READY status", upgraded)
	}
	if db.Migrator().HasIndex(&snapshot.RepositorySnapshot{}, "uq_repo_commit") {
		t.Fatal("legacy repository+commit identity index still exists")
	}
	if !db.Migrator().HasIndex(&snapshot.RepositorySnapshot{}, "uq_repo_commit_revision") {
		t.Fatal("revision-scoped snapshot identity index was not created")
	}
	newRevisionSnapshot := &snapshot.RepositorySnapshot{
		ID: "revision-snapshot", RepositoryID: legacy.RepositoryID, AnalysisRevisionID: "new-revision",
		CommitSHA: legacy.CommitSHA, Ref: "main", MaterializedPath: "/tmp/new-source",
		ContentHash: "new-hash", Status: snapshot.StatusMaterializing,
	}
	if err := db.Create(newRevisionSnapshot).Error; err != nil {
		t.Fatalf("create new Revision snapshot for existing commit: %v", err)
	}
}
