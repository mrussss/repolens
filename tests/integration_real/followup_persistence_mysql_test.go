package integration_real

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"repolens/internal/analysispipeline"
	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
)

func TestRealMySQL_DiagnosisDescriptionRedactionCapacity(t *testing.T) {
	db, jobsStore, cleanup := setupRealMySQL(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	repoStore := repo.NewStore(db)
	snapshotStore := snapshot.NewStore(db)
	const repoID, snapshotID = "repo-diagnosis-capacity", "snapshot-diagnosis-capacity"
	if err := repoStore.Create(ctx, &repo.Repository{
		ID: repoID, UserID: "capacity-user", Name: "capacity", GitURL: "https://github.com/example/capacity", DefaultRef: "main", Status: repo.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := snapshotStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapshotID, RepositoryID: repoID, CommitSHA: "0123456789012345678901234567890123456789", Ref: "main", Status: snapshot.StatusReady,
	}); err != nil {
		t.Fatal(err)
	}
	buildContext := codeintelmodel.DefaultBuildContext()
	if err := db.Create(&codeintelmodel.CodeIndexBuild{
		ID: 8001, SnapshotID: snapshotID, ParserVersion: codeintelmodel.CurrentParserVersion,
		AnalyzerVersion: codeintelmodel.CurrentAnalyzerVersion, SymbolSchemaVersion: codeintelmodel.CurrentSymbolSchemaVersion,
		BuildContextHash: buildContext.BuildContextHash(), ModulePath: "example.com/capacity", GOOS: buildContext.GOOS,
		GOARCH: buildContext.GOARCH, BuildTagsHash: buildContext.BuildTagsHash(), Status: codeintelmodel.BuildStatusReady,
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&codeintelmodel.RetrievalBuild{
		ID: 8002, CodeIndexBuildID: 8001, Strategy: "BM25", RetrievalVersion: codeintelmodel.CurrentRetrievalVersion,
		TokenizerVersion: codeintelmodel.CurrentTokenizerVersion, ConfigHash: "capacity", ArtifactPath: "capacity-index",
		ArtifactHash: "capacity-hash", Status: codeintelmodel.BuildStatusReady,
	}).Error; err != nil {
		t.Fatal(err)
	}
	service := diagnosis.NewService(diagnosis.ServiceDependencies{
		Store: diagnosis.NewStore(db), RepoStore: repoStore,
		Lineage: analysispipeline.NewResolver(revision.NewStore(db), snapshotStore, codeintelstore.NewStore(db)),
	})
	input := diagnosis.CreateDiagnosisInput{
		UserID: "capacity-user", RepositoryID: repoID, SnapshotID: snapshotID,
		IssueTitle: "large but permitted description", IssueDescription: strings.Repeat("x", 65_536),
		IdempotencyKey: "description-at-mediumtext-boundary", CodeIndexBuildID: 8001, RetrievalBuildID: 8002,
	}
	if err := diagnosis.ValidateInput(input); err != nil {
		t.Fatalf("65,536-byte issue description should satisfy the explicit raw input limit: %v", err)
	}
	run, created, err := service.Create(ctx, input)
	if err != nil || !created {
		t.Fatalf("create with 65,536-byte description: created=%t err=%v", created, err)
	}
	var stored diagnosis.DiagnosisRun
	if err := db.First(&stored, "id = ?", run.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.IssueDescription != input.IssueDescription {
		t.Fatal("MySQL description round trip changed accepted input")
	}
	if len(run.IssueDescription) != 65_536 {
		t.Fatalf("stored description bytes=%d; want 65,536", len(run.IssueDescription))
	}
	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil || job.Status != jobs.StatusPending {
		t.Fatalf("diagnosis job=%+v err=%v; want a durable PENDING job", job, err)
	}

	var issueDescriptionType string
	if err := db.Raw(`SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'diagnosis_runs' AND COLUMN_NAME = 'issue_description'`).Scan(&issueDescriptionType).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(issueDescriptionType, "mediumtext") {
		t.Fatalf("diagnosis_runs.issue_description type=%q; want MEDIUMTEXT", issueDescriptionType)
	}

	// Each short credential expands to the fixed redaction marker. Raw bytes fit
	// the documented limit, while the final persisted bytes exceed that limit.
	input.IssueTitle = "redaction expansion"
	input.IdempotencyKey = "description-redaction-expansion"
	input.IssueDescription = strings.Repeat("Authorization: a x\n", 3000)
	if len(input.IssueDescription) > diagnosis.MaxIssueDescriptionBytes || len(diagnosis.RedactSecrets(input.IssueDescription)) <= diagnosis.MaxIssueDescriptionBytes {
		t.Fatalf("redaction fixture raw=%d redacted=%d; want raw within limit and redacted above it", len(input.IssueDescription), len(diagnosis.RedactSecrets(input.IssueDescription)))
	}
	if err := db.Exec(`CREATE TRIGGER reject_unvalidated_diagnosis BEFORE INSERT ON diagnosis_runs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'oversized redacted input reached INSERT'`).Error; err != nil {
		t.Fatal(err)
	}
	_, _, err = service.Create(ctx, input)
	_ = db.Exec(`DROP TRIGGER IF EXISTS reject_unvalidated_diagnosis`)
	if err != diagnosis.ErrInputTooLarge {
		t.Fatalf("redaction expansion error=%v; want stable ErrInputTooLarge before INSERT", err)
	}
	var count int64
	if err := db.Model(&diagnosis.DiagnosisRun{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("diagnosis rows=%d after rejected redaction expansion; want only the original row", count)
	}

	input.IdempotencyKey = "description-raw-limit"
	input.IssueDescription = strings.Repeat("x", diagnosis.MaxIssueDescriptionBytes+1)
	if _, _, err := service.Create(ctx, input); err != diagnosis.ErrInputTooLarge {
		t.Fatalf("raw description above product limit error=%v; want ErrInputTooLarge", err)
	}
	input.IssueDescription = "normal description"
	input.ErrorLog = strings.Repeat("x", diagnosis.MaxErrorLogBytes)
	input.IdempotencyKey = "log-boundary"
	logRun, _, err := service.Create(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	stored = diagnosis.DiagnosisRun{}
	if err := db.First(&stored, "id = ?", logRun.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.ErrorLog != input.ErrorLog {
		t.Fatal("accepted error log did not survive MySQL persistence")
	}
	input.IdempotencyKey = "log-expansion"
	input.ErrorLog = strings.Repeat("Authorization: a x\n", 12000)
	if len(input.ErrorLog) > diagnosis.MaxErrorLogBytes || len(diagnosis.RedactSecrets(input.ErrorLog)) <= diagnosis.MaxErrorLogBytes {
		t.Fatal("log expansion fixture does not cross persisted boundary")
	}
	if _, _, err := service.Create(ctx, input); err != diagnosis.ErrInputTooLarge {
		t.Fatalf("expanded log error=%v", err)
	}
	input.ErrorLog = strings.Repeat("x", diagnosis.MaxErrorLogBytes+1)
	if _, _, err := service.Create(ctx, input); err != diagnosis.ErrInputTooLarge {
		t.Fatalf("raw log error=%v", err)
	}

}

func TestRealMySQL_LargeDocumentationCodeIndexReachesReady(t *testing.T) {
	db, jobsStore, cleanup := setupRealMySQL(t)
	t.Cleanup(cleanup)
	ctx := context.Background()
	const repoID, snapshotID, modulePath = "repo-long-doc", "snapshot-long-doc", "example.com/longdoc"
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	sourceDir, err := storeFS.EnsureDir(repoID, snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte("module "+modulePath+"\n\ngo 1.22\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var source strings.Builder
	source.WriteString("package longdoc\n\n")
	var expectedDoc strings.Builder
	for i := 0; i < 720; i++ {
		fmt.Fprintf(&source, "// doc-row-%04d %s\n", i, strings.Repeat("x", 96))
		fmt.Fprintf(&expectedDoc, "doc-row-%04d %s\n", i, strings.Repeat("x", 96))
	}
	source.WriteString("func LongDocumented() string { return \"ready\" }\n")
	if err := os.WriteFile(filepath.Join(sourceDir, "longdoc.go"), []byte(source.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	readyAt := time.Now().UTC()
	if err := snapshot.NewStore(db).Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapshotID, RepositoryID: repoID, CommitSHA: "0123456789012345678901234567890123456789", Ref: "main",
		MaterializedPath: sourceDir, Status: snapshot.StatusReady, ReadyAt: &readyAt,
	}); err != nil {
		t.Fatal(err)
	}
	codeIndexStore := codeintelstore.NewStore(db)
	build, created, err := codeIndexStore.GetOrCreateBuild(ctx, snapshotID, modulePath, codeintelmodel.DefaultBuildContext())
	if err != nil || !created {
		t.Fatalf("create CodeIndexBuild=%+v created=%t err=%v", build, created, err)
	}
	handler := codeintel.NewCodeIndexJobHandler(codeIndexStore, snapshot.NewStore(db), storeFS, codeintel.NewAnalyzer())
	worker := startLegacyHandoffWorker(t, jobsStore, jobs.JobTypeBuildCodeIndex, handler)
	worker.RegisterHandler(jobs.JobTypeBuildRetrieval, jobs.HandlerFunc(func(context.Context, *jobs.AnalysisJob) error { return nil }))
	jobID := childBuildJobID(t, jobsStore, jobs.JobTypeBuildCodeIndex, fmt.Sprintf("%d", build.ID))
	if job := waitForRealLegacyJobStatus(t, jobsStore, jobID, jobs.StatusSucceeded); job.Status != jobs.StatusSucceeded {
		t.Fatalf("CodeIndex build job status=%s; want SUCCEEDED", job.Status)
	}
	readyBuild, err := codeIndexStore.GetByID(ctx, build.ID)
	if err != nil || readyBuild.Status != codeintelmodel.BuildStatusReady {
		t.Fatalf("CodeIndexBuild=%+v err=%v; want READY", readyBuild, err)
	}
	symbols, err := codeIndexStore.ListAllSymbols(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	var documented *codeintelmodel.Symbol
	for _, sym := range symbols {
		if sym.Name == "LongDocumented" {
			documented = sym
			break
		}
	}
	if documented == nil || len(documented.Doc) <= 65_535 {
		t.Fatalf("persisted long doc symbol=%+v; expected more than 65,535 bytes", documented)
	}
	if documented.Doc != expectedDoc.String() {
		t.Fatal("MySQL did not preserve the complete 720-line documentation")
	}
	if strings.Contains(documented.Signature, "doc-row-") || documented.Signature != "func LongDocumented() string" {
		t.Fatalf("persisted signature contains documentation or body: %q", documented.Signature)
	}
	var signatureType, docType string
	if err := db.Raw(`SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'symbols' AND COLUMN_NAME = 'signature'`).Scan(&signatureType).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Raw(`SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'symbols' AND COLUMN_NAME = 'doc'`).Scan(&docType).Error; err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(signatureType, "mediumtext") || !strings.EqualFold(docType, "mediumtext") {
		t.Fatalf("symbols.signature=%q symbols.doc=%q; want both MEDIUMTEXT", signatureType, docType)
	}
}

func TestRealMySQL_Migration019ResumesAfterPartialDDL(t *testing.T) {
	db, sqlDB, cleanup := setupRealMySQLDatabase(t)
	t.Cleanup(cleanup)
	migrationDB := &mysql.DB{GormDB: db, SqlDB: sqlDB}
	allMigrations := filepath.Join("..", "..", "migrations")
	pre019Dir := t.TempDir()
	entries, err := os.ReadDir(allMigrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() >= "019_v2_2_persisted_text_capacity.sql" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(allMigrations, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(pre019Dir, entry.Name()), contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := mysql.ApplyMigrations(migrationDB, pre019Dir); err != nil {
		t.Fatalf("apply migrations through 018: %v", err)
	}
	legacyDescription := "existing diagnosis description"
	if err := db.Exec(`INSERT INTO diagnosis_runs (id, user_id, repository_id, snapshot_id, issue_title, issue_description, idempotency_key, idempotency_request_hash)
		VALUES ('migration-capacity-run', 'migration-user', 'migration-repo', 'migration-snapshot', 'issue', ?, 'migration-key', 'migration-hash')`, legacyDescription).Error; err != nil {
		t.Fatal(err)
	}
	legacySignature, legacyDoc := "func Existing()", "existing symbol documentation"
	if err := db.Exec(`INSERT INTO symbols (code_index_build_id, file_id, file_path, symbol_key_raw, symbol_key_hash, module_path, package_path, package_name, kind, name, qualified_name, signature, doc, start_line, start_col, end_line, end_col, content_hash)
		VALUES (1, 1, 'existing.go', 'existing-key', 'existing-hash', 'example.com/existing', 'example.com/existing', 'existing', 'FUNCTION', 'Existing', 'existing.Existing', ?, ?, 1, 1, 1, 1, 'existing-content')`, legacySignature, legacyDoc).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`ALTER TABLE diagnosis_runs MODIFY COLUMN issue_description MEDIUMTEXT NULL`).Error; err != nil {
		t.Fatalf("simulate first migration DDL already committed: %v", err)
	}
	if err := mysql.ApplyMigrations(migrationDB, allMigrations); err != nil {
		t.Fatalf("resume migration 019 after partial DDL: %v", err)
	}
	if err := mysql.ApplyMigrations(migrationDB, allMigrations); err != nil {
		t.Fatalf("repeat migration 019: %v", err)
	}
	var storedDescription string
	if err := db.Raw(`SELECT issue_description FROM diagnosis_runs WHERE id = 'migration-capacity-run'`).Scan(&storedDescription).Error; err != nil {
		t.Fatal(err)
	}
	if storedDescription != legacyDescription {
		t.Fatalf("migration changed existing issue_description %q", storedDescription)
	}
	var storedSignature, storedDoc string
	if err := db.Raw(`SELECT signature, doc FROM symbols WHERE symbol_key_hash = 'existing-hash'`).Row().Scan(&storedSignature, &storedDoc); err != nil {
		t.Fatal(err)
	}
	if storedSignature != legacySignature || storedDoc != legacyDoc {
		t.Fatalf("migration changed existing symbol signature=%q doc=%q", storedSignature, storedDoc)
	}
	for _, column := range []struct{ table, name string }{
		{table: "diagnosis_runs", name: "issue_description"},
		{table: "symbols", name: "signature"},
		{table: "symbols", name: "doc"},
	} {
		var columnType string
		if err := db.Raw(`SELECT COLUMN_TYPE FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`, column.table, column.name).Scan(&columnType).Error; err != nil {
			t.Fatal(err)
		}
		if !strings.EqualFold(columnType, "mediumtext") {
			t.Errorf("%s.%s=%q; want MEDIUMTEXT", column.table, column.name, columnType)
		}
	}
}
