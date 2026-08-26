package mysql

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
)

func AutoMigrate(db *gorm.DB) error {
	return db.AutoMigrate(
		&repo.Repository{},
		&snapshot.RepositorySnapshot{},
		&codeintelmodel.CodeIndexBuild{},
		&codeintelmodel.RetrievalBuild{},
		&codeintelmodel.CodeFile{},
		&codeintelmodel.Symbol{},
		&codeintelmodel.SymbolRelation{},
		&repoindex.RepositoryIndex{},
		&diagnosis.DiagnosisRun{},
		&diagnosis.DiagnosisAttempt{},
		&evidence.Report{},
		&evidence.Citation{},
		&trace.AgentStep{},
		&jobs.AnalysisJob{},
	)
}

// ApplyMigrations is the production schema entry point. SQL files are the
// source of truth; AutoMigrate is intentionally retained only for sqlite
// development/test databases.
func ApplyMigrations(db *DB, dir string) error {
	if db == nil || db.SqlDB == nil {
		return fmt.Errorf("database is nil")
	}
	if db.GormDB.Dialector.Name() != "mysql" {
		return AutoMigrate(db.GormDB)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			files = append(files, filepath.Join(dir, entry.Name()))
		}
	}
	sort.Strings(files)
	if _, err := db.SqlDB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version VARCHAR(128) PRIMARY KEY, applied_at DATETIME(3) NOT NULL)`); err != nil {
		return err
	}
	for _, file := range files {
		version := filepath.Base(file)
		var count int
		if err := db.SqlDB.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		tx, err := db.SqlDB.Begin()
		if err != nil {
			return err
		}
		var sqlLines []string
		for _, line := range strings.Split(string(contents), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "--") {
				sqlLines = append(sqlLines, line)
			}
		}
		for _, statement := range strings.Split(strings.Join(sqlLines, "\n"), ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			if _, err := tx.Exec(statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply %s: %w", version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (?, CURRENT_TIMESTAMP(3))`, version); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
