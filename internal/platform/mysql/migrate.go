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
	// SQLite development databases can predate AnalysisRevision and contain
	// NULL lineage IDs. Normalize those values before GORM rebuilds the table
	// for the new NOT NULL composite identity; doing this afterward is too late
	// because SQLite copies old rows during the rebuild.
	if db.Dialector.Name() == "sqlite" &&
		db.Migrator().HasTable(&snapshot.RepositorySnapshot{}) &&
		db.Migrator().HasColumn(&snapshot.RepositorySnapshot{}, "analysis_revision_id") {
		if err := db.Model(&snapshot.RepositorySnapshot{}).
			Where("analysis_revision_id IS NULL").
			Update("analysis_revision_id", "").Error; err != nil {
			return fmt.Errorf("normalize legacy sqlite snapshot revision IDs: %w", err)
		}
	}
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
	// Older sqlite development databases may still have the pre-revision
	// repository+commit unique index. Drop it after AutoMigrate has created the
	// revision-scoped replacement so the same commit can be prepared again when
	// the analysis pipeline identity changes.
	if db.Migrator().HasIndex(&snapshot.RepositorySnapshot{}, "uq_repo_commit") {
		if err := db.Migrator().DropIndex(&snapshot.RepositorySnapshot{}, "uq_repo_commit"); err != nil {
			return fmt.Errorf("drop legacy snapshot identity index: %w", err)
		}
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
		if version == "012_v2_2_code_index_build_tags.sql" {
			if err := applyBuildTagsMigration012(ctx, conn); err != nil {
				return fmt.Errorf("apply resumable migration %s: %w", version, err)
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, CURRENT_TIMESTAMP(3))`, version); err != nil {
				return fmt.Errorf("record migration %s: %w", version, err)
			}
			continue
		}
		if version == "013_v2_2_revision_snapshot_identity.sql" {
			if err := applyRevisionSnapshotIdentityMigration013(ctx, conn); err != nil {
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

func applyRevisionSnapshotIdentityMigration013(ctx context.Context, conn *sql.Conn) error {
	var columnType, nullable string
	var defaultValue sql.NullString
	err := conn.QueryRowContext(ctx, `SELECT COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'repository_snapshots' AND COLUMN_NAME = 'analysis_revision_id'`).Scan(&columnType, &nullable, &defaultValue)
	if err != nil {
		return fmt.Errorf("inspect repository_snapshots.analysis_revision_id: %w", err)
	}
	if !strings.EqualFold(columnType, "varchar(36)") {
		return fmt.Errorf("repository_snapshots.analysis_revision_id has incompatible type %s", columnType)
	}
	if _, err := conn.ExecContext(ctx, `UPDATE repository_snapshots SET analysis_revision_id = '' WHERE analysis_revision_id IS NULL`); err != nil {
		return fmt.Errorf("normalize legacy empty snapshot revision identities: %w", err)
	}
	if !strings.EqualFold(nullable, "NO") || !defaultValue.Valid || strings.Trim(defaultValue.String, "'") != "" {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE repository_snapshots MODIFY COLUMN analysis_revision_id VARCHAR(36) NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("make snapshot revision identity non-null: %w", err)
		}
	}

	if err := ensureMigration013Index(ctx, conn, "uq_snapshot_repo_commit_revision", true, []string{"repository_id", "commit_sha", "analysis_revision_id"}); err != nil {
		return err
	}
	if err := dropMigration013LegacyIndex(ctx, conn); err != nil {
		return err
	}
	return nil
}

func ensureMigration013Index(ctx context.Context, conn *sql.Conn, name string, unique bool, wantColumns []string) error {
	columns, nonUnique, exists, err := migration013Index(ctx, conn, name)
	if err != nil {
		return err
	}
	if exists {
		if (nonUnique == 0) != unique || !sameStrings(columns, wantColumns) {
			return fmt.Errorf("index %s has incompatible definition: unique=%t columns=%v", name, nonUnique == 0, columns)
		}
		return nil
	}
	if !unique {
		return fmt.Errorf("cannot create unexpected non-unique migration index %s", name)
	}
	if _, err := conn.ExecContext(ctx, `CREATE UNIQUE INDEX uq_snapshot_repo_commit_revision ON repository_snapshots (repository_id, commit_sha, analysis_revision_id)`); err != nil {
		return fmt.Errorf("create revision-scoped snapshot identity index: %w", err)
	}
	columns, nonUnique, exists, err = migration013Index(ctx, conn, name)
	if err != nil {
		return err
	}
	if !exists || nonUnique != 0 || !sameStrings(columns, wantColumns) {
		return fmt.Errorf("revision-scoped snapshot identity index verification failed")
	}
	return nil
}

func dropMigration013LegacyIndex(ctx context.Context, conn *sql.Conn) error {
	const name = "uq_snapshot_repo_commit"
	columns, nonUnique, exists, err := migration013Index(ctx, conn, name)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	if nonUnique != 0 || !sameStrings(columns, []string{"repository_id", "commit_sha"}) {
		return fmt.Errorf("legacy index %s has unexpected definition: unique=%t columns=%v", name, nonUnique == 0, columns)
	}
	if _, err := conn.ExecContext(ctx, `DROP INDEX uq_snapshot_repo_commit ON repository_snapshots`); err != nil {
		return fmt.Errorf("drop legacy snapshot identity index: %w", err)
	}
	return nil
}

func migration013Index(ctx context.Context, conn *sql.Conn, name string) ([]string, int, bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME, NON_UNIQUE FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'repository_snapshots' AND INDEX_NAME = ? ORDER BY SEQ_IN_INDEX`, name)
	if err != nil {
		return nil, 0, false, fmt.Errorf("inspect repository_snapshots index %s: %w", name, err)
	}
	defer rows.Close()
	var columns []string
	nonUnique := -1
	for rows.Next() {
		var column string
		var currentNonUnique int
		if err := rows.Scan(&column, &currentNonUnique); err != nil {
			return nil, 0, false, err
		}
		if nonUnique >= 0 && nonUnique != currentNonUnique {
			return nil, 0, false, fmt.Errorf("index %s has inconsistent uniqueness metadata", name)
		}
		nonUnique = currentNonUnique
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, err
	}
	return columns, nonUnique, nonUnique >= 0, nil
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func applyBuildTagsMigration012(ctx context.Context, conn *sql.Conn) error {
	var count int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'code_index_builds' AND COLUMN_NAME = 'build_tags_json'`).Scan(&count); err != nil {
		return fmt.Errorf("inspect code_index_builds.build_tags_json: %w", err)
	}
	if count == 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE code_index_builds ADD COLUMN build_tags_json TEXT NULL`); err != nil {
			return fmt.Errorf("add code_index_builds.build_tags_json: %w", err)
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE code_index_builds SET build_tags_json = '[]' WHERE build_tags_json IS NULL OR build_tags_json = ''`); err != nil {
		return fmt.Errorf("initialize code index build tags: %w", err)
	}
	// Historical pending builds persisted the tag hash but not the tag names.
	// The hash is one-way, so fail those jobs closed. READY rows remain immutable
	// historical artifacts; the v2.2 parser/pipeline identity prevents them from
	// being reused for new preparations.
	if _, err := conn.ExecContext(ctx, `UPDATE code_index_builds
		SET status = 'FAILED', error_code = 'BUILD_TAGS_UNAVAILABLE'
		WHERE status IN ('CREATED', 'BUILDING')
		  AND (build_tags_hash IS NULL OR build_tags_hash <> 'e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855')
		  AND COALESCE(error_code, '') <> 'BUILD_TAGS_UNAVAILABLE'`); err != nil {
		return fmt.Errorf("retire code index builds with unavailable legacy tags: %w", err)
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
