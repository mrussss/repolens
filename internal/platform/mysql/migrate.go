package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"

	codeintelmodel "repolens/internal/codeintel/model"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
)

func AutoMigrate(db *gorm.DB) error {
	if err := db.AutoMigrate(
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
		&revision.AnalysisRevision{},
		&evidence.Report{},
		&evidence.Citation{},
		&evidence.AttemptEvidenceItem{},
		&trace.AgentStep{},
		&jobs.AnalysisJob{},
	); err != nil {
		return err
	}
	return db.Model(&diagnosis.DiagnosisAttempt{}).
		Where("checkpoint_kind = ? AND (raw_output <> '' OR finish_reason <> '')", diagnosis.CheckpointKindNone).
		Update("checkpoint_kind", diagnosis.CheckpointKindLegacyUntyped).Error
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := db.SqlDB.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	const lockName = "repolens:schema_migrations"
	var lockResult sql.NullInt64
	if err := conn.QueryRowContext(ctx, `SELECT GET_LOCK(?, 30)`, lockName).Scan(&lockResult); err != nil {
		return fmt.Errorf("acquire migration lock: %w", err)
	}
	if !lockResult.Valid || lockResult.Int64 != 1 {
		return fmt.Errorf("timed out acquiring MySQL migration lock")
	}
	defer func() {
		var released sql.NullInt64
		_ = conn.QueryRowContext(context.Background(), `SELECT RELEASE_LOCK(?)`, lockName).Scan(&released)
	}()

	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version VARCHAR(128) PRIMARY KEY, applied_at DATETIME(3) NOT NULL)`); err != nil {
		return err
	}
	for _, file := range files {
		version := filepath.Base(file)
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			continue
		}
		contents, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		if version == "009_v2_2_rc_recovery.sql" {
			if err := applyRecoveryMigration009(ctx, conn); err != nil {
				return fmt.Errorf("apply resumable migration %s: %w", version, err)
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, CURRENT_TIMESTAMP(3))`, version); err != nil {
				return fmt.Errorf("record migration %s: %w", version, err)
			}
			continue
		}
		tx, err := conn.BeginTx(ctx, nil)
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
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("apply %s: %w", version, err)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, CURRENT_TIMESTAMP(3))`, version); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

type migration009Column struct {
	name       string
	columnType string
	nullable   string
	defaultVal *string
	definition string
}

func applyRecoveryMigration009(ctx context.Context, conn *sql.Conn) error {
	none := "NONE"
	one := "1"
	columns := []migration009Column{
		{name: "execution_generation", columnType: "int", nullable: "NO", defaultVal: &one, definition: "INT NOT NULL DEFAULT 1 AFTER diagnosis_run_id"},
		{name: "checkpoint_kind", columnType: "varchar(32)", nullable: "NO", defaultVal: &none, definition: "VARCHAR(32) NOT NULL DEFAULT 'NONE' AFTER parsed_report_draft_json"},
		{name: "checkpoint_error_code", columnType: "varchar(64)", nullable: "YES", definition: "VARCHAR(64) NULL AFTER checkpoint_kind"},
		{name: "checkpoint_error_message", columnType: "varchar(255)", nullable: "YES", definition: "VARCHAR(255) NULL AFTER checkpoint_error_code"},
	}
	for _, column := range columns {
		if err := ensureMigration009Column(ctx, conn, column); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE diagnosis_attempts SET checkpoint_kind = 'LEGACY_UNTYPED' WHERE checkpoint_kind = 'NONE' AND (raw_output <> '' OR finish_reason <> '')`); err != nil {
		return fmt.Errorf("backfill legacy checkpoint kinds: %w", err)
	}

	if err := ensureMigration009AttemptIndex(ctx, conn); err != nil {
		return err
	}

	var untyped int64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM diagnosis_attempts WHERE checkpoint_kind = 'NONE' AND (raw_output <> '' OR finish_reason <> '')`).Scan(&untyped); err != nil {
		return err
	}
	if untyped != 0 {
		return fmt.Errorf("legacy checkpoint backfill incomplete")
	}
	return nil
}

func ensureMigration009Column(ctx context.Context, conn *sql.Conn, want migration009Column) error {
	var columnType, nullable string
	var defaultValue sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'diagnosis_attempts' AND COLUMN_NAME = ?`, want.name).Scan(&columnType, &nullable, &defaultValue)
	if err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("inspect diagnosis_attempts.%s: %w", want.name, err)
	}
	if err == sql.ErrNoRows {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE diagnosis_attempts ADD COLUMN `+want.name+` `+want.definition); err != nil {
			return fmt.Errorf("add diagnosis_attempts.%s: %w", want.name, err)
		}
		if err := conn.QueryRowContext(ctx, `SELECT COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'diagnosis_attempts' AND COLUMN_NAME = ?`, want.name).Scan(&columnType, &nullable, &defaultValue); err != nil {
			return fmt.Errorf("verify diagnosis_attempts.%s: %w", want.name, err)
		}
	}
	if !migration009TypeMatches(columnType, want.columnType) || !strings.EqualFold(nullable, want.nullable) || !migration009DefaultMatches(defaultValue, want.defaultVal) {
		return fmt.Errorf("diagnosis_attempts.%s has incompatible definition (type=%s nullable=%s)", want.name, columnType, nullable)
	}
	return nil
}

func migration009TypeMatches(got, want string) bool {
	got = strings.ToLower(strings.TrimSpace(got))
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "int" {
		return got == "int" || strings.HasPrefix(got, "int(")
	}
	return got == want
}

func migration009DefaultMatches(got sql.NullString, want *string) bool {
	if want == nil {
		return !got.Valid
	}
	return got.Valid && strings.Trim(got.String, "'") == *want
}

func ensureMigration009AttemptIndex(ctx context.Context, conn *sql.Conn) error {
	rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME, NON_UNIQUE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'diagnosis_attempts' AND INDEX_NAME = 'uq_attempt_run_generation_no' ORDER BY SEQ_IN_INDEX`)
	if err != nil {
		return fmt.Errorf("inspect attempt generation index: %w", err)
	}
	var columns []string
	unique := true
	for rows.Next() {
		var column string
		var nonUnique int
		if err := rows.Scan(&column, &nonUnique); err != nil {
			_ = rows.Close()
			return err
		}
		columns = append(columns, column)
		unique = unique && nonUnique == 0
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	want := []string{"diagnosis_run_id", "execution_generation", "attempt_no"}
	if len(columns) > 0 {
		if !unique || strings.Join(columns, ",") != strings.Join(want, ",") {
			return fmt.Errorf("index uq_attempt_run_generation_no exists with an incompatible definition")
		}
		return nil
	}
	var duplicates int64
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT diagnosis_run_id, execution_generation, attempt_no FROM diagnosis_attempts GROUP BY diagnosis_run_id, execution_generation, attempt_no HAVING COUNT(*) > 1) AS duplicate_attempts`).Scan(&duplicates); err != nil {
		return fmt.Errorf("check duplicate Attempt identities: %w", err)
	}
	if duplicates != 0 {
		return fmt.Errorf("cannot create uq_attempt_run_generation_no: duplicate Attempt identities exist")
	}
	if _, err := conn.ExecContext(ctx, `ALTER TABLE diagnosis_attempts ADD UNIQUE KEY uq_attempt_run_generation_no (diagnosis_run_id, execution_generation, attempt_no)`); err != nil {
		return fmt.Errorf("create uq_attempt_run_generation_no: %w", err)
	}
	return nil
}
