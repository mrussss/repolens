package integration_real

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/evidence"
	"repolens/internal/platform/mysql"
)

func TestRealMySQL_ForwardMigrationsRecoverAfterPartialDDL(t *testing.T) {
	allMigrations := filepath.Join("..", "..", "migrations")
	preFixMigrations := t.TempDir()
	entries, err := os.ReadDir(allMigrations)
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "010_v2_2_citation_reason.sql" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(allMigrations, entry.Name()))
		if err != nil {
			t.Fatalf("read migration %s: %v", entry.Name(), err)
		}
		if err := os.WriteFile(filepath.Join(preFixMigrations, entry.Name()), contents, 0600); err != nil {
			t.Fatalf("copy migration %s: %v", entry.Name(), err)
		}
	}

	db, _, cleanup := setupRealMySQL(t, preFixMigrations)
	if cleanup != nil {
		defer cleanup()
	}
	if db == nil {
		return
	}
	var columnType string
	if err := db.Raw(`SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'citations' AND COLUMN_NAME = 'reason'`).Scan(&columnType).Error; err != nil {
		t.Fatalf("inspect pre-upgrade Citation.reason: %v", err)
	}
	if !strings.EqualFold(columnType, "varchar(255)") {
		t.Fatalf("pre-upgrade Citation.reason type = %s, want varchar(255)", columnType)
	}
	var chunkIDType string
	if err := db.Raw(`SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'attempt_evidence_items' AND COLUMN_NAME = 'retrieval_chunk_id'`).Scan(&chunkIDType).Error; err != nil {
		t.Fatalf("inspect pre-upgrade retrieval_chunk_id: %v", err)
	}
	if !strings.EqualFold(chunkIDType, "varchar(128)") {
		t.Fatalf("pre-upgrade retrieval_chunk_id type = %s, want varchar(128)", chunkIDType)
	}
	legacyCustomContext := codeintelmodel.BuildContext{GOOS: "linux", GOARCH: "amd64", BuildTags: []string{"legacy_custom"}}
	legacyDefaultContext := codeintelmodel.BuildContext{GOOS: "linux", GOARCH: "amd64"}
	if err := db.Exec(`INSERT INTO code_index_builds (snapshot_id, parser_version, analyzer_version, symbol_schema_version, build_context_hash, module_path, goos, goarch, build_tags_hash, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"snap-legacy-build-tags", "v2.1.0", "v2.1.0", "v2.1.0", legacyCustomContext.BuildContextHash(), "example.com/legacy", "linux", "amd64", legacyCustomContext.BuildTagsHash(), "CREATED",
		"snap-legacy-default-tags", "v2.1.0", "v2.1.0", "v2.1.0", legacyDefaultContext.BuildContextHash(), "example.com/legacy", "linux", "amd64", legacyDefaultContext.BuildTagsHash(), "CREATED").Error; err != nil {
		t.Fatalf("seed pre-upgrade code index build: %v", err)
	}
	if err := db.Exec(`INSERT INTO code_index_builds (snapshot_id, parser_version, analyzer_version, symbol_schema_version, build_context_hash, module_path, goos, goarch, build_tags_hash, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"snap-legacy-ready-build-tags", "v2.1.0", "v2.1.0", "v2.1.0", legacyCustomContext.BuildContextHash(), "example.com/legacy", "linux", "amd64", legacyCustomContext.BuildTagsHash(), "READY").Error; err != nil {
		t.Fatalf("seed pre-upgrade READY custom-tag build: %v", err)
	}
	var legacyPendingBuildID int64
	if err := db.Raw(`SELECT id FROM code_index_builds WHERE snapshot_id = ?`, "snap-legacy-build-tags").Row().Scan(&legacyPendingBuildID); err != nil {
		t.Fatalf("read pre-upgrade pending build ID: %v", err)
	}
	var readyLegacyBuildID int64
	if err := db.Raw(`SELECT id FROM code_index_builds WHERE snapshot_id = ?`, "snap-legacy-ready-build-tags").Row().Scan(&readyLegacyBuildID); err != nil {
		t.Fatalf("read pre-upgrade READY build ID: %v", err)
	}
	if err := db.Exec(`INSERT INTO analysis_revisions (id, repository_id, source_ref, commit_sha, pipeline_version, pipeline_fingerprint, code_index_build_id, status, stage) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"legacy-ready-revision", "legacy-repo", "main", strings.Repeat("a", 40), "v2.2", "legacy-fingerprint", readyLegacyBuildID, "READY", "READY").Error; err != nil {
		t.Fatalf("seed revision pinned to legacy READY build: %v", err)
	}
	// Simulate an interrupted forward migration: MySQL committed its DDL, but
	// the migration runner did not yet record version 010.
	if err := db.Exec(`ALTER TABLE citations MODIFY reason VARCHAR(2048) NULL`).Error; err != nil {
		t.Fatalf("simulate interrupted citation reason migration: %v", err)
	}
	if err := db.Exec(`ALTER TABLE attempt_evidence_items MODIFY retrieval_chunk_id VARCHAR(1024) NULL`).Error; err != nil {
		t.Fatalf("simulate interrupted retrieval chunk ID migration: %v", err)
	}
	if err := db.Exec(`ALTER TABLE code_index_builds ADD COLUMN build_tags_json TEXT NULL`).Error; err != nil {
		t.Fatalf("simulate interrupted build tags migration: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get MySQL sql.DB: %v", err)
	}
	if err := mysql.ApplyMigrations(&mysql.DB{GormDB: db, SqlDB: sqlDB}, allMigrations); err != nil {
		t.Fatalf("apply citation reason forward migration: %v", err)
	}
	if err := db.Raw(`SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'citations' AND COLUMN_NAME = 'reason'`).Scan(&columnType).Error; err != nil {
		t.Fatalf("inspect upgraded Citation.reason: %v", err)
	}
	if !strings.EqualFold(columnType, "varchar(2048)") {
		t.Fatalf("upgraded Citation.reason type = %s, want varchar(2048)", columnType)
	}
	if err := db.Raw(`SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'attempt_evidence_items' AND COLUMN_NAME = 'retrieval_chunk_id'`).Scan(&chunkIDType).Error; err != nil {
		t.Fatalf("inspect upgraded retrieval_chunk_id: %v", err)
	}
	if !strings.EqualFold(chunkIDType, "varchar(1024)") {
		t.Fatalf("upgraded retrieval_chunk_id type = %s, want varchar(1024)", chunkIDType)
	}
	var buildTagsType string
	if err := db.Raw(`SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'code_index_builds' AND COLUMN_NAME = 'build_tags_json'`).Scan(&buildTagsType).Error; err != nil {
		t.Fatalf("inspect upgraded build_tags_json: %v", err)
	}
	if !strings.EqualFold(buildTagsType, "text") {
		t.Fatalf("upgraded build_tags_json type = %s, want text", buildTagsType)
	}
	var backfilledTags string
	if err := db.Raw(`SELECT build_tags_json FROM code_index_builds WHERE snapshot_id = ?`, "snap-legacy-build-tags").Scan(&backfilledTags).Error; err != nil {
		t.Fatalf("read backfilled legacy build tags: %v", err)
	}
	if backfilledTags != "[]" {
		t.Fatalf("legacy build tags = %q, want []", backfilledTags)
	}
	var legacyStatus, legacyErrorCode, legacyContextHash string
	if err := db.Raw(`SELECT status, error_code, build_context_hash FROM code_index_builds WHERE snapshot_id = ?`, "snap-legacy-build-tags").Row().Scan(&legacyStatus, &legacyErrorCode, &legacyContextHash); err != nil {
		t.Fatalf("read retired legacy custom-tag build: %v", err)
	}
	if legacyStatus != "FAILED" || legacyErrorCode != "BUILD_TAGS_UNAVAILABLE" || legacyContextHash != legacyCustomContext.BuildContextHash() {
		t.Fatalf("legacy custom-tag build was not retired safely: status=%s error=%s hash=%s", legacyStatus, legacyErrorCode, legacyContextHash)
	}
	var legacyReadyStatus, legacyReadyError string
	if err := db.Raw(`SELECT status, COALESCE(error_code, '') FROM code_index_builds WHERE snapshot_id = ?`, "snap-legacy-ready-build-tags").Row().Scan(&legacyReadyStatus, &legacyReadyError); err != nil {
		t.Fatalf("read preserved legacy READY custom-tag build: %v", err)
	}
	if legacyReadyStatus != "READY" || legacyReadyError != "" {
		t.Fatalf("legacy READY build was not preserved as historical data: status=%s error=%s", legacyReadyStatus, legacyReadyError)
	}
	var revisionStatus, revisionError string
	if err := db.Raw(`SELECT status, COALESCE(error_code, '') FROM analysis_revisions WHERE id = ?`, "legacy-ready-revision").Row().Scan(&revisionStatus, &revisionError); err != nil {
		t.Fatalf("read retired legacy analysis revision: %v", err)
	}
	if revisionStatus != "READY" || revisionError != "" {
		t.Fatalf("analysis revision pinned to legacy READY build was not preserved: status=%s error=%s", revisionStatus, revisionError)
	}
	var defaultStatus, defaultTags string
	if err := db.Raw(`SELECT status, build_tags_json FROM code_index_builds WHERE snapshot_id = ?`, "snap-legacy-default-tags").Row().Scan(&defaultStatus, &defaultTags); err != nil {
		t.Fatalf("read legacy default-tag build: %v", err)
	}
	if defaultStatus != "CREATED" || defaultTags != "[]" {
		t.Fatalf("legacy default-tag build changed unexpectedly: status=%s tags=%q", defaultStatus, defaultTags)
	}
	replacement, created, err := codeintelstore.NewStore(db).GetOrCreateBuild(context.Background(), "snap-legacy-build-tags", "example.com/legacy", legacyCustomContext)
	if err != nil {
		t.Fatalf("create replacement for retired custom-tag build: %v", err)
	}
	if !created || replacement.ID == legacyPendingBuildID || replacement.ParserVersion != codeintelmodel.CurrentParserVersion || replacement.BuildContextHash != legacyCustomContext.BuildContextHash() || replacement.BuildTagsJSON != `["legacy_custom"]` {
		t.Fatalf("replacement build = %+v, created=%t", replacement, created)
	}
	readyReplacement, readyCreated, err := codeintelstore.NewStore(db).GetOrCreateBuild(context.Background(), "snap-legacy-ready-build-tags", "example.com/legacy", legacyCustomContext)
	if err != nil {
		t.Fatalf("create current-version build alongside legacy READY build: %v", err)
	}
	if !readyCreated || readyReplacement.ID == readyLegacyBuildID || readyReplacement.ParserVersion != codeintelmodel.CurrentParserVersion {
		t.Fatalf("legacy READY build was reused for current parser version: %+v created=%t", readyReplacement, readyCreated)
	}

	reason := strings.Repeat("r", 2048)
	reportData := &evidence.DiagnosisReportData{
		ConclusionKind: evidence.ConclusionRootCause,
		Summary:        "summary",
		RootCause:      "root cause",
		Findings: []evidence.Finding{{
			Title: "finding", Reasoning: "reasoning",
			Citations: []evidence.Citation{{FilePath: "pkg/source.go", Reason: reason}},
		}},
	}
	if err := evidence.ValidateReportStructure(reportData); err != nil {
		t.Fatalf("2048-byte citation reason should pass report validation: %v", err)
	}
	tooLong := *reportData
	tooLong.Findings = append([]evidence.Finding(nil), reportData.Findings...)
	tooLong.Findings[0].Citations = append([]evidence.Citation(nil), reportData.Findings[0].Citations...)
	tooLong.Findings[0].Citations[0].Reason = reason + "x"
	if err := evidence.ValidateReportStructure(&tooLong); err == nil {
		t.Fatal("2049-byte citation reason unexpectedly passed report validation")
	}

	report := &evidence.Report{
		DiagnosisRunID: "run-citation-long-reason", AttemptID: "attempt-citation-long-reason",
		RootCause: "root cause", FindingsJSON: "[]", RecommendedChecksJSON: "[]",
	}
	if err := evidence.NewReportStore(db).Create(context.Background(), report); err != nil {
		t.Fatalf("persist validated report: %v", err)
	}
	citations := []evidence.Citation{{
		ReportID: report.ID, SnapshotID: "snap-citation-long-reason", CodeIndexBuildID: 1,
		FilePath: "pkg/source.go", StartLine: 1, EndLine: 1, Reason: reason,
		ValidationStatus: evidence.CitationUnchecked,
	}}
	store := evidence.NewCitationStore(db)
	if err := store.CreateBatch(context.Background(), citations); err != nil {
		t.Fatalf("persist 2048-byte Citation.reason: %v", err)
	}
	loaded, err := store.ListByReportID(context.Background(), report.ID)
	if err != nil {
		t.Fatalf("read persisted citations: %v", err)
	}
	if len(loaded) != 1 || loaded[0].Reason != reason {
		t.Fatalf("persisted reason mismatch: rows=%d reason_bytes=%d", len(loaded), len(loaded[0].Reason))
	}
}
