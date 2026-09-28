package integration_real

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"repolens/internal/repo"
)

func TestRealMySQLSourceRefLengths(t *testing.T) {
	db, _, cleanup := setupRealMySQL(t)
	if cleanup != nil {
		defer cleanup()
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	var sqlMode string
	if err := conn.QueryRowContext(ctx, `SELECT @@SESSION.sql_mode`).Scan(&sqlMode); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(sqlMode), "STRICT") {
		if _, err := conn.ExecContext(ctx, `SET SESSION sql_mode = CONCAT(@@SESSION.sql_mode, ',STRICT_ALL_TABLES')`); err != nil {
			t.Fatalf("enable strict MySQL mode for boundary test: %v", err)
		}
	}

	for _, column := range []struct{ table, name string }{
		{table: "repositories", name: "default_ref"},
		{table: "repository_snapshots", name: "ref"},
		{table: "repository_snapshots", name: "requested_ref"},
	} {
		var columnType string
		if err := conn.QueryRowContext(ctx, `SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, column.table, column.name).Scan(&columnType); err != nil {
			t.Fatalf("inspect %s.%s: %v", column.table, column.name, err)
		}
		if !strings.EqualFold(columnType, "varchar(255)") {
			t.Errorf("%s.%s type = %s, want VARCHAR(255)", column.table, column.name, columnType)
		}
	}

	for _, length := range []int{128, 129, 255, 256} {
		for _, target := range []struct{ table, column string }{
			{table: "repositories", column: "default_ref"},
			{table: "repository_snapshots", column: "ref"},
			{table: "repository_snapshots", column: "requested_ref"},
		} {
			t.Run(fmt.Sprintf("%s.%s/%d", target.table, target.column, length), func(t *testing.T) {
				ref := strings.Repeat("r", length)
				var insertErr error
				switch target.table + "." + target.column {
				case "repositories.default_ref":
					id := fmt.Sprintf("repo-ref-%s-%d", target.column, length)
					_, insertErr = conn.ExecContext(ctx, `INSERT INTO repositories (id, user_id, name, git_url, default_ref, status) VALUES (?, 'user-ref-test', 'ref boundary', ?, ?, 'ACTIVE')`, id, "https://example.com/"+id, ref)
				case "repository_snapshots.ref":
					id := fmt.Sprintf("snapshot-ref-%d", length)
					_, insertErr = conn.ExecContext(ctx, `INSERT INTO repository_snapshots (id, repository_id, commit_sha, analysis_revision_id, ref, materialized_path, content_hash, status) VALUES (?, ?, ?, '', ?, '/tmp/ref', '', 'CREATED')`, id, "repo-snapshot-ref", "commit-ref-"+fmt.Sprint(length), ref)
				case "repository_snapshots.requested_ref":
					id := fmt.Sprintf("snapshot-requested-ref-%d", length)
					_, insertErr = conn.ExecContext(ctx, `INSERT INTO repository_snapshots (id, repository_id, commit_sha, analysis_revision_id, ref, requested_ref, materialized_path, content_hash, status) VALUES (?, ?, ?, '', 'main', ?, '/tmp/ref', '', 'CREATED')`, id, "repo-requested-ref", "commit-requested-"+fmt.Sprint(length), ref)
				}
				if length <= repo.MaxGitRefLength && insertErr != nil {
					t.Fatalf("insert length %d: %v", length, insertErr)
				}
				if length > repo.MaxGitRefLength && insertErr == nil {
					t.Fatal("length 256 unexpectedly stored in VARCHAR(255)")
				}
			})
		}
	}
}
