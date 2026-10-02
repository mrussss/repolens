package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

type migrationColumnDefinition struct {
	typeName      string
	nullable      bool
	defaultValue  sql.NullString
	autoIncrement bool
}

type migrationIndexDefinition struct {
	name      string
	columns   []string
	nonUnique int
}

type migrationSchemaColumn struct {
	columnType   string
	nullable     string
	defaultValue sql.NullString
	extra        string
}

var (
	migrationTypePattern         = regexp.MustCompile(`(?i)^\s*([a-z]+(?:\s+unsigned)?(?:\s*\(\s*\d+(?:\s*,\s*\d+)?\s*\))?)`)
	migrationDefaultPattern      = regexp.MustCompile(`(?i)\bDEFAULT\s+('(?:''|[^'])*'|CURRENT_TIMESTAMP(?:\(\d+\))?|[^\s,]+)`)
	migrationAddColumnPattern    = regexp.MustCompile("(?is)^ADD\\s+(?:COLUMN\\s+)?`?([a-z0-9_]+)`?\\s+(.+)$")
	migrationModifyColumnPattern = regexp.MustCompile("(?is)^MODIFY\\s+(?:COLUMN\\s+)?`?([a-z0-9_]+)`?\\s+(.+)$")
	migrationAddIndexPattern     = regexp.MustCompile("(?is)^ADD\\s+(?:(UNIQUE)\\s+)?(?:KEY|INDEX)\\s+`?([a-z0-9_]+)`?\\s*\\((.*)\\)")
	migrationAlterDefaultPattern = regexp.MustCompile("(?is)^ALTER\\s+COLUMN\\s+`?([a-z0-9_]+)`?\\s+SET\\s+DEFAULT\\s+(.+)$")
	migrationCreateTablePattern  = regexp.MustCompile("(?is)^CREATE\\s+TABLE\\s+(?:IF\\s+NOT\\s+EXISTS\\s+)?`?([a-z0-9_]+)`?")
)

// applyResumableMigration executes a MySQL migration statement by statement.
// MySQL commits DDL independently of the migration ledger, so each DDL
// operation is inspected before it is run and verified after it is run.
func applyResumableMigration(ctx context.Context, conn *sql.Conn, version, contents string) error {
	for _, statement := range splitMigrationStatements(contents) {
		upper := strings.ToUpper(strings.TrimSpace(statement))
		switch {
		case strings.HasPrefix(upper, "CREATE TABLE"):
			if err := applyCreateTableResumable(ctx, conn, version, statement); err != nil {
				return err
			}
		case strings.HasPrefix(upper, "ALTER TABLE"):
			if err := applyAlterTableResumable(ctx, conn, version, statement); err != nil {
				return err
			}
		default:
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("execute migration statement: %w", err)
			}
		}
	}
	return nil
}

func splitMigrationStatements(contents string) []string {
	var lines []string
	for _, line := range strings.Split(contents, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			lines = append(lines, line)
		}
	}
	var statements []string
	for _, statement := range strings.Split(strings.Join(lines, "\n"), ";") {
		if statement = strings.TrimSpace(statement); statement != "" {
			statements = append(statements, statement)
		}
	}
	return statements
}

func applyCreateTableResumable(ctx context.Context, conn *sql.Conn, version, statement string) error {
	m := migrationCreateTablePattern.FindStringSubmatch(statement)
	if m == nil {
		return schemaConflict(version, "CREATE TABLE", "unrecognized statement", "known table definition")
	}
	table := strings.ToLower(m[1])
	exists, err := migrationTableExists(ctx, conn, table)
	if err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	}
	body, err := createTableBody(statement)
	if err != nil {
		return schemaConflict(version, table, err.Error(), "valid CREATE TABLE definition")
	}
	columns, indexes, err := parseCreateTableBody(body)
	if err != nil {
		return schemaConflict(version, table, err.Error(), "valid CREATE TABLE definition")
	}
	if !exists {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("create table %s: %w", table, err)
		}
	}
	for name, want := range columns {
		got, err := inspectMigrationColumn(ctx, conn, table, name)
		if err != nil {
			return schemaConflict(version, table+"."+name, err.Error(), want.String())
		}
		if !columnDefinitionMatches(got, want) {
			return schemaConflict(version, table+"."+name, got.String(), want.String())
		}
	}
	for _, want := range indexes {
		if err := verifyMigrationIndex(ctx, conn, table, want); err != nil {
			return schemaConflict(version, table+"."+want.name, err.Error(), want.String())
		}
	}
	return nil
}

func createTableBody(statement string) (string, error) {
	start := strings.Index(statement, "(")
	if start < 0 {
		return "", fmt.Errorf("missing column list")
	}
	end, err := matchingSQLParen(statement, start)
	if err != nil {
		return "", err
	}
	return statement[start+1 : end], nil
}

func parseCreateTableBody(body string) (map[string]migrationColumnDefinition, []migrationIndexDefinition, error) {
	columns := make(map[string]migrationColumnDefinition)
	var indexes []migrationIndexDefinition
	for _, item := range splitSQLTopLevel(body, ',') {
		item = strings.TrimSpace(item)
		upper := strings.ToUpper(item)
		if strings.HasPrefix(upper, "CONSTRAINT ") || strings.HasPrefix(upper, "CHECK ") || strings.HasPrefix(upper, "FOREIGN KEY") {
			continue
		}
		if index, ok, err := parseCreateIndex(item); err != nil {
			return nil, nil, err
		} else if ok {
			indexes = append(indexes, index)
			continue
		}
		name, definition, err := parseNamedColumnDefinition(item)
		if err != nil {
			return nil, nil, err
		}
		parsed, err := parseMigrationColumnDefinition(definition)
		if err != nil {
			return nil, nil, fmt.Errorf("column %s: %w", name, err)
		}
		columns[name] = parsed
		if strings.Contains(strings.ToUpper(definition), "PRIMARY KEY") {
			indexes = append(indexes, migrationIndexDefinition{name: "PRIMARY", columns: []string{name}, nonUnique: 0})
		}
	}
	return columns, indexes, nil
}

func parseCreateIndex(item string) (migrationIndexDefinition, bool, error) {
	trimmed := strings.TrimSpace(item)
	upper := strings.ToUpper(trimmed)
	unique, primary := false, false
	var tail string
	switch {
	case strings.HasPrefix(upper, "PRIMARY KEY"):
		primary = true
		tail = strings.TrimSpace(trimmed[len("PRIMARY KEY"):])
	case strings.HasPrefix(upper, "UNIQUE KEY "):
		unique = true
		tail = strings.TrimSpace(trimmed[len("UNIQUE KEY "):])
	case strings.HasPrefix(upper, "UNIQUE INDEX "):
		unique = true
		tail = strings.TrimSpace(trimmed[len("UNIQUE INDEX "):])
	case strings.HasPrefix(upper, "KEY "):
		tail = strings.TrimSpace(trimmed[len("KEY "):])
	case strings.HasPrefix(upper, "INDEX "):
		tail = strings.TrimSpace(trimmed[len("INDEX "):])
	default:
		return migrationIndexDefinition{}, false, nil
	}
	name := "PRIMARY"
	if !primary {
		var err error
		name, tail, err = takeSQLIdentifier(tail)
		if err != nil {
			return migrationIndexDefinition{}, false, err
		}
	}
	open := strings.Index(tail, "(")
	if open < 0 {
		return migrationIndexDefinition{}, false, fmt.Errorf("index %s has no column list", name)
	}
	close, err := matchingSQLParen(tail, open)
	if err != nil {
		return migrationIndexDefinition{}, false, err
	}
	var cols []string
	for _, col := range splitSQLTopLevel(tail[open+1:close], ',') {
		col = strings.TrimSpace(strings.Trim(col, "`"))
		if space := strings.IndexAny(col, " \t"); space >= 0 {
			col = col[:space]
		}
		cols = append(cols, strings.ToLower(col))
	}
	nonUnique := 1
	if unique || primary {
		nonUnique = 0
	}
	return migrationIndexDefinition{name: strings.ToLower(name), columns: cols, nonUnique: nonUnique}, true, nil
}

func applyAlterTableResumable(ctx context.Context, conn *sql.Conn, version, statement string) error {
	prefix := regexp.MustCompile(`(?is)^ALTER\s+TABLE\s+`)
	body := prefix.ReplaceAllString(strings.TrimSpace(statement), "")
	table, rest, err := takeSQLIdentifier(body)
	if err != nil {
		return schemaConflict(version, "ALTER TABLE", err.Error(), "known table")
	}
	table = strings.ToLower(table)
	for _, clause := range splitSQLTopLevel(rest, ',') {
		clause = strings.TrimSpace(clause)
		switch {
		case migrationAddIndexPattern.MatchString(clause):
			if err := applyAddIndex(ctx, conn, version, table, clause); err != nil {
				return err
			}
		case migrationAddColumnPattern.MatchString(clause):
			if err := applyAddColumn(ctx, conn, version, table, clause); err != nil {
				return err
			}
		case migrationModifyColumnPattern.MatchString(clause):
			if err := applyModifyColumn(ctx, conn, version, table, clause); err != nil {
				return err
			}
		case migrationAlterDefaultPattern.MatchString(clause):
			if err := applyAlterDefault(ctx, conn, version, table, clause); err != nil {
				return err
			}
		default:
			return schemaConflict(version, table, "unsupported DDL: "+clause, "a resumable ADD/MODIFY/ALTER operation")
		}
	}
	return nil
}

func applyAddColumn(ctx context.Context, conn *sql.Conn, version, table, clause string) error {
	m := migrationAddColumnPattern.FindStringSubmatch(clause)
	name := strings.ToLower(m[1])
	want, err := parseMigrationColumnDefinition(m[2])
	if err != nil {
		return schemaConflict(version, table+"."+name, err.Error(), "valid column definition")
	}
	if exists, err := migrationTableExists(ctx, conn, table); err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	} else if !exists {
		return schemaConflict(version, table, "table is missing", "table containing column "+name)
	}
	got, err := inspectMigrationColumn(ctx, conn, table, name)
	if err == nil {
		if !columnDefinitionMatches(got, want) {
			return schemaConflict(version, table+"."+name, got.String(), want.String())
		}
		return nil
	}
	if err != sql.ErrNoRows {
		return fmt.Errorf("inspect %s.%s: %w", table, name, err)
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE "+table+" "+clause); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, name, err)
	}
	got, err = inspectMigrationColumn(ctx, conn, table, name)
	if err != nil || !columnDefinitionMatches(got, want) {
		if err != nil {
			return fmt.Errorf("verify added %s.%s: %w", table, name, err)
		}
		return schemaConflict(version, table+"."+name, got.String(), want.String())
	}
	return nil
}

func applyModifyColumn(ctx context.Context, conn *sql.Conn, version, table, clause string) error {
	m := migrationModifyColumnPattern.FindStringSubmatch(clause)
	name := strings.ToLower(m[1])
	want, err := parseMigrationColumnDefinition(m[2])
	if err != nil {
		return schemaConflict(version, table+"."+name, err.Error(), "valid column definition")
	}
	got, err := inspectMigrationColumn(ctx, conn, table, name)
	if err != nil {
		return schemaConflict(version, table+"."+name, err.Error(), want.String())
	}
	if columnDefinitionMatches(got, want) {
		return nil
	}
	legacy, ok := migrationLegacyColumnDefinition(version, table, name)
	if !ok || !columnDefinitionMatches(got, legacy) {
		return schemaConflict(version, table+"."+name, got.String(), want.String())
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE "+table+" "+clause); err != nil {
		return fmt.Errorf("modify %s.%s: %w", table, name, err)
	}
	got, err = inspectMigrationColumn(ctx, conn, table, name)
	if err != nil || !columnDefinitionMatches(got, want) {
		if err != nil {
			return fmt.Errorf("verify modified %s.%s: %w", table, name, err)
		}
		return schemaConflict(version, table+"."+name, got.String(), want.String())
	}
	return nil
}

func applyAddIndex(ctx context.Context, conn *sql.Conn, version, table, clause string) error {
	m := migrationAddIndexPattern.FindStringSubmatch(clause)
	nonUnique := 1
	if strings.TrimSpace(m[1]) != "" {
		nonUnique = 0
	}
	want := migrationIndexDefinition{name: strings.ToLower(m[2]), columns: parseIndexColumns(m[3]), nonUnique: nonUnique}
	if exists, err := migrationTableExists(ctx, conn, table); err != nil {
		return fmt.Errorf("inspect table %s: %w", table, err)
	} else if !exists {
		return schemaConflict(version, table, "table is missing", "table containing index "+want.name)
	}
	if err := verifyMigrationIndex(ctx, conn, table, want); err == nil {
		return nil
	} else if !strings.Contains(err.Error(), "does not exist") {
		return schemaConflict(version, table+"."+want.name, err.Error(), want.String())
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE "+table+" "+clause); err != nil {
		return fmt.Errorf("add index %s.%s: %w", table, want.name, err)
	}
	if err := verifyMigrationIndex(ctx, conn, table, want); err != nil {
		return schemaConflict(version, table+"."+want.name, err.Error(), want.String())
	}
	return nil
}

func applyAlterDefault(ctx context.Context, conn *sql.Conn, version, table, clause string) error {
	m := migrationAlterDefaultPattern.FindStringSubmatch(clause)
	name := strings.ToLower(m[1])
	wantDefault := normalizeDefault(m[2])
	got, err := inspectMigrationColumn(ctx, conn, table, name)
	if err != nil {
		return schemaConflict(version, table+"."+name, err.Error(), "default "+wantDefault)
	}
	if got.defaultValue.Valid && normalizeDefault(got.defaultValue.String) == wantDefault {
		return nil
	}
	if version != "008_v2_2_production_agent_config.sql" || table != "diagnosis_runs" || name != "max_output_tokens" || !got.defaultValue.Valid || normalizeDefault(got.defaultValue.String) != "2048" {
		return schemaConflict(version, table+"."+name, got.String(), "default "+wantDefault)
	}
	if _, err := conn.ExecContext(ctx, "ALTER TABLE "+table+" "+clause); err != nil {
		return fmt.Errorf("change default for %s.%s: %w", table, name, err)
	}
	got, err = inspectMigrationColumn(ctx, conn, table, name)
	if err != nil || !got.defaultValue.Valid || normalizeDefault(got.defaultValue.String) != wantDefault {
		if err != nil {
			return fmt.Errorf("verify default for %s.%s: %w", table, name, err)
		}
		return schemaConflict(version, table+"."+name, got.String(), "default "+wantDefault)
	}
	return nil
}

func migrationLegacyColumnDefinition(version, table, name string) (migrationColumnDefinition, bool) {
	legacy := map[string]string{
		"010_v2_2_citation_reason.sql|citations|reason":                                    "VARCHAR(255) NULL",
		"011_v2_2_retrieval_chunk_id_length.sql|attempt_evidence_items|retrieval_chunk_id": "VARCHAR(128) NULL",
		"014_v2_2_symbol_relation_reason_detail_text.sql|symbol_relations|reason_detail":   "VARCHAR(255) NULL",
		"015_v2_2_source_ref_length.sql|repositories|default_ref":                          "VARCHAR(128) NOT NULL DEFAULT 'main'",
		"015_v2_2_source_ref_length.sql|repository_snapshots|ref":                          "VARCHAR(128) NOT NULL",
		"015_v2_2_source_ref_length.sql|repository_snapshots|requested_ref":                "VARCHAR(128) NULL",
	}
	definition, ok := legacy[version+"|"+table+"|"+name]
	if !ok {
		return migrationColumnDefinition{}, false
	}
	parsed, err := parseMigrationColumnDefinition(definition)
	return parsed, err == nil
}

func parseNamedColumnDefinition(item string) (string, string, error) {
	name, rest, err := takeSQLIdentifier(strings.TrimSpace(item))
	if err != nil {
		return "", "", err
	}
	return strings.ToLower(name), strings.TrimSpace(rest), nil
}

func parseMigrationColumnDefinition(definition string) (migrationColumnDefinition, error) {
	m := migrationTypePattern.FindStringSubmatch(strings.TrimSpace(definition))
	if m == nil {
		return migrationColumnDefinition{}, fmt.Errorf("missing column type in %q", definition)
	}
	typeName := normalizeColumnType(m[1])
	upper := strings.ToUpper(definition)
	nullable := !strings.Contains(upper, "NOT NULL") && !strings.Contains(upper, "PRIMARY KEY")
	defaultValue := sql.NullString{}
	if d := migrationDefaultPattern.FindStringSubmatch(definition); d != nil {
		value := strings.TrimSpace(d[1])
		if !strings.EqualFold(value, "NULL") {
			defaultValue = sql.NullString{String: normalizeDefault(value), Valid: true}
		}
	}
	return migrationColumnDefinition{
		typeName: typeName, nullable: nullable, defaultValue: defaultValue,
		autoIncrement: strings.Contains(upper, "AUTO_INCREMENT"),
	}, nil
}

func inspectMigrationColumn(ctx context.Context, conn *sql.Conn, table, column string) (migrationSchemaColumn, error) {
	var got migrationSchemaColumn
	err := conn.QueryRowContext(ctx, `SELECT COLUMN_TYPE, IS_NULLABLE, COLUMN_DEFAULT, EXTRA FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, table, column).
		Scan(&got.columnType, &got.nullable, &got.defaultValue, &got.extra)
	if err != nil {
		return migrationSchemaColumn{}, err
	}
	return got, nil
}

func migrationTableExists(ctx context.Context, conn *sql.Conn, table string) (bool, error) {
	var count int
	err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&count)
	return count > 0, err
}

func columnDefinitionMatches(got migrationSchemaColumn, want migrationColumnDefinition) bool {
	if normalizeColumnType(got.columnType) != want.typeName || !strings.EqualFold(got.nullable, nullableText(want.nullable)) {
		return false
	}
	if want.autoIncrement != hasAutoIncrement(got.extra) && want.autoIncrement {
		return false
	}
	if got.defaultValue.Valid != want.defaultValue.Valid {
		return false
	}
	return !got.defaultValue.Valid || normalizeDefault(got.defaultValue.String) == normalizeDefault(want.defaultValue.String)
}

func (d migrationSchemaColumn) String() string {
	return fmt.Sprintf("type=%s nullable=%s default=%s auto_increment=%t", normalizeColumnType(d.columnType), strings.ToUpper(d.nullable), nullString(d.defaultValue), hasAutoIncrement(d.extra))
}

func (d migrationColumnDefinition) String() string {
	return fmt.Sprintf("type=%s nullable=%s default=%s auto_increment=%t", d.typeName, nullableText(d.nullable), nullString(d.defaultValue), d.autoIncrement)
}

func hasAutoIncrement(extra string) bool {
	for _, attribute := range strings.Fields(strings.ToLower(extra)) {
		if attribute == "auto_increment" {
			return true
		}
	}
	return false
}

func nullString(value sql.NullString) string {
	if !value.Valid {
		return "NULL"
	}
	return value.String
}

func nullableText(nullable bool) string {
	if nullable {
		return "YES"
	}
	return "NO"
}

func normalizeColumnType(value string) string {
	value = strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
	if strings.HasPrefix(value, "int(") && strings.HasSuffix(value, ")") {
		return "int"
	}
	if value == "boolean" || value == "bool" {
		return "tinyint(1)"
	}
	return value
}

func normalizeDefault(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		value = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	switch strings.ToLower(value) {
	case "false":
		return "0"
	case "true":
		return "1"
	default:
		value = strings.ToLower(value)
		return strings.TrimSuffix(value, ".0")
	}
}

func parseIndexColumns(value string) []string {
	var columns []string
	for _, column := range splitSQLTopLevel(value, ',') {
		column = strings.TrimSpace(strings.Trim(column, "`"))
		if space := strings.IndexAny(column, " \t"); space >= 0 {
			column = column[:space]
		}
		columns = append(columns, strings.ToLower(column))
	}
	return columns
}

func verifyMigrationIndex(ctx context.Context, conn *sql.Conn, table string, want migrationIndexDefinition) error {
	rows, err := conn.QueryContext(ctx, `SELECT COLUMN_NAME, NON_UNIQUE FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ? ORDER BY SEQ_IN_INDEX`, table, want.name)
	if err != nil {
		return err
	}
	defer rows.Close()
	var columns []string
	nonUnique := -1
	for rows.Next() {
		var column string
		var currentNonUnique int
		if err := rows.Scan(&column, &currentNonUnique); err != nil {
			return err
		}
		columns = append(columns, strings.ToLower(column))
		if nonUnique >= 0 && nonUnique != currentNonUnique {
			return fmt.Errorf("inconsistent uniqueness metadata")
		}
		nonUnique = currentNonUnique
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(columns) == 0 {
		return fmt.Errorf("index does not exist")
	}
	if nonUnique != want.nonUnique || !sameStrings(columns, want.columns) {
		return fmt.Errorf("existing index unique=%t columns=%v", nonUnique == 0, columns)
	}
	return nil
}

func (d migrationIndexDefinition) String() string {
	return fmt.Sprintf("unique=%t columns=%v", d.nonUnique == 0, d.columns)
}

func schemaConflict(version, object, got, want string) error {
	return fmt.Errorf("migration schema conflict in %s for %s: got %s; want %s", version, object, got, want)
}

func takeSQLIdentifier(input string) (string, string, error) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", "", fmt.Errorf("missing SQL identifier")
	}
	if input[0] == '`' {
		end := strings.Index(input[1:], "`")
		if end < 0 {
			return "", "", fmt.Errorf("unterminated quoted SQL identifier")
		}
		end++
		return input[1:end], strings.TrimSpace(input[end+1:]), nil
	}
	end := strings.IndexAny(input, " \t\r\n")
	if end < 0 {
		return strings.Trim(input, "`"), "", nil
	}
	return strings.Trim(input[:end], "`"), strings.TrimSpace(input[end:]), nil
}

func matchingSQLParen(input string, open int) (int, error) {
	depth := 0
	var quote rune
	for i, r := range input[open:] {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return open + i, nil
			}
		}
	}
	return 0, fmt.Errorf("unbalanced SQL parentheses")
}

func splitSQLTopLevel(input string, delimiter rune) []string {
	var parts []string
	start, depth := 0, 0
	var quote rune
	for i, r := range input {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"', '`':
			quote = r
		case '(':
			depth++
		case ')':
			depth--
		case delimiter:
			if depth == 0 {
				parts = append(parts, input[start:i])
				start = i + 1
			}
		}
	}
	return append(parts, input[start:])
}
