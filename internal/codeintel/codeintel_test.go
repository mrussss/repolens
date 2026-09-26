package codeintel_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/snapshot"
	"repolens/internal/tools"
)

func setupCodeIntelTestDB(t *testing.T) (*gorm.DB, *jobs.Store, codeintelstore.Store, snapshot.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "codeintel_test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed opening sqlite db: %v", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed migrating db: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed getting sql.DB: %v", err)
	}

	jobsStore := jobs.NewStoreWithDriver(sqlDB, "sqlite3")
	ciStore := codeintelstore.NewStore(db)
	snapStore := snapshot.NewStore(db)
	return db, jobsStore, ciStore, snapStore
}

func TestRelatedTestsPersistAndReachFindRelatedTestsTool(t *testing.T) {
	_, _, ciStore, _ := setupCodeIntelTestDB(t)
	ctx := context.Background()
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "go.mod"), "module example.com/related\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "orders.go"), `package related

func ProcessOrder() {}
`)
	writeTestFile(t, filepath.Join(root, "orders_test.go"), `package related

func TestProcessOrder() {
	ProcessOrder()
}
`)

	analysis, err := codeintel.NewAnalyzer().Analyze(ctx, root, codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("analyze fixture: %v", err)
	}
	if len(analysis.RelatedTests) == 0 {
		t.Fatal("analyzer did not discover related tests")
	}

	build, _, err := ciStore.GetOrCreateBuild(ctx, "snap-related-tests", analysis.ModulePath, analysis.BuildContext)
	if err != nil {
		t.Fatalf("create code index build: %v", err)
	}
	if err := ciStore.MarkBuildBuilding(ctx, build.ID); err != nil {
		t.Fatalf("mark code index build building: %v", err)
	}
	if err := ciStore.SaveAnalysisResult(ctx, build.ID, analysis); err != nil {
		t.Fatalf("save analysis result: %v", err)
	}

	discovery := analysis.RelatedTests[0]
	relations, err := ciStore.ListRelatedTests(ctx, build.ID, discovery.TargetSymbolKeyHash)
	if err != nil {
		t.Fatalf("list related tests: %v", err)
	}
	if len(relations) != 1 {
		t.Fatalf("stored related tests = %d, want 1: %+v", len(relations), relations)
	}
	got := relations[0]
	if got.CodeIndexBuildID != build.ID || got.FromSymbolID == nil || got.ToSymbolID == nil ||
		got.FromSymbolKeyHash != discovery.TargetSymbolKeyHash || got.ToSymbolKeyHash != discovery.TestSymbolKeyHash ||
		got.RelationType != codeintelmodel.RelationTypeTestRelation || got.ResolutionKind != discovery.ResolutionKind ||
		got.Confidence != discovery.Confidence || got.ReasonCode != string(discovery.ReasonCode) ||
		got.ReasonDetail != discovery.Explanation || got.FilePath != discovery.TestFilePath || got.Line != discovery.TestLine || got.FileID == 0 {
		t.Fatalf("persisted relation does not preserve discovery: discovery=%+v relation=%+v", discovery, got)
	}

	toolResult, err := tools.NewFindRelatedTestsTool(ciStore, build.ID).Execute(ctx, `{"symbol_name":"ProcessOrder"}`)
	if err != nil {
		t.Fatalf("find_related_tests tool: %v", err)
	}
	var toolRelations []codeintelmodel.SymbolRelation
	if err := json.Unmarshal([]byte(toolResult), &toolRelations); err != nil {
		t.Fatalf("decode find_related_tests output %q: %v", toolResult, err)
	}
	if len(toolRelations) != 1 || !strings.Contains(toolResult, discovery.TestSymbolKeyHash) || !strings.Contains(toolResult, discovery.TestFilePath) {
		t.Fatalf("find_related_tests did not expose persisted relation: %s", toolResult)
	}
}

func TestAnalyzerSeparatesExternalTestPackageIdentityAndPersistsRelations(t *testing.T) {
	_, _, ciStore, _ := setupCodeIntelTestDB(t)
	ctx := context.Background()
	root := t.TempDir()
	modulePath := "example.com/symbolidentity"
	writeTestFile(t, filepath.Join(root, "go.mod"), "module "+modulePath+"\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(root, "foo.go"), `package p

func Helper() {}

func Invoke() { Helper() }
`)
	writeTestFile(t, filepath.Join(root, "foo_test.go"), `package p_test

import (
	"testing"
	p "example.com/symbolidentity"
)

func Helper() {}

func TestExternal(t *testing.T) { p.Helper() }
`)
	writeTestFile(t, filepath.Join(root, "foo_internal_test.go"), `package p

import "testing"

func TestInternal(t *testing.T) { Helper() }
`)

	analysis, err := codeintel.NewAnalyzer().Analyze(ctx, root, codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("analyze package identity fixture: %v", err)
	}
	var productionHelper, externalHelper, externalTest, internalTest *codeintelmodel.Symbol
	for _, symbol := range analysis.Symbols {
		switch {
		case symbol.Name == "Helper" && symbol.FilePath == "foo.go":
			productionHelper = symbol
		case symbol.Name == "Helper" && symbol.FilePath == "foo_test.go":
			externalHelper = symbol
		case symbol.Name == "TestExternal":
			externalTest = symbol
		case symbol.Name == "TestInternal":
			internalTest = symbol
		}
	}
	if productionHelper == nil || externalHelper == nil || externalTest == nil || internalTest == nil {
		t.Fatalf("fixture symbols missing: production=%v external=%v externalTest=%v internalTest=%v", productionHelper, externalHelper, externalTest, internalTest)
	}
	if productionHelper.PackagePath != modulePath || externalHelper.PackagePath != modulePath+"_test" {
		t.Fatalf("package identities = production %q, external test %q; want %q and %q",
			productionHelper.PackagePath, externalHelper.PackagePath, modulePath, modulePath+"_test")
	}
	if productionHelper.SymbolKeyHash == externalHelper.SymbolKeyHash || productionHelper.SymbolKeyRaw == externalHelper.SymbolKeyRaw {
		t.Fatalf("production and external-test Helper share identity: production=%+v external=%+v", productionHelper, externalHelper)
	}
	if analysis.Quality.PackagesTotal != 2 || analysis.Quality.PackagesTypechecked != 2 {
		t.Fatalf("package type-check grouping = total %d, checked %d; want two distinct checked packages",
			analysis.Quality.PackagesTotal, analysis.Quality.PackagesTypechecked)
	}

	var externalCallResolvedToProduction, internalCallResolvedToProduction bool
	for _, relation := range analysis.Relations {
		if relation.RelationType != codeintelmodel.RelationTypeCallCandidate || relation.ToSymbolKeyHash != productionHelper.SymbolKeyHash {
			continue
		}
		if relation.FromSymbolKeyHash == externalTest.SymbolKeyHash {
			externalCallResolvedToProduction = true
		}
		if relation.FromSymbolKeyHash == internalTest.SymbolKeyHash {
			internalCallResolvedToProduction = true
		}
	}
	if !externalCallResolvedToProduction {
		t.Fatal("external-test p.Helper() relation did not resolve to the production Helper symbol")
	}
	if !internalCallResolvedToProduction {
		t.Fatal("same-package test Helper() relation did not resolve to the production Helper symbol")
	}

	build, _, err := ciStore.GetOrCreateBuild(ctx, "snap-symbol-identity", analysis.ModulePath, analysis.BuildContext)
	if err != nil {
		t.Fatalf("create CodeIndexBuild: %v", err)
	}
	if err := ciStore.MarkBuildBuilding(ctx, build.ID); err != nil {
		t.Fatalf("mark CodeIndexBuild building: %v", err)
	}
	if err := ciStore.SaveAnalysisResult(ctx, build.ID, analysis); err != nil {
		t.Fatalf("persist CodeIndex analysis: %v", err)
	}
	ready, err := ciStore.GetByID(ctx, build.ID)
	if err != nil || ready.Status != codeintelmodel.BuildStatusReady {
		t.Fatalf("CodeIndexBuild status = %v, err=%v; want READY", ready, err)
	}

	persistedSymbols, err := ciStore.ListAllSymbols(ctx, build.ID)
	if err != nil {
		t.Fatalf("list persisted symbols: %v", err)
	}
	var persistedProductionHelper, persistedExternalHelper *codeintelmodel.Symbol
	for _, symbol := range persistedSymbols {
		if symbol.Name != "Helper" {
			continue
		}
		switch symbol.FilePath {
		case "foo.go":
			persistedProductionHelper = symbol
		case "foo_test.go":
			persistedExternalHelper = symbol
		}
	}
	if persistedProductionHelper == nil || persistedExternalHelper == nil ||
		persistedProductionHelper.SymbolKeyHash == persistedExternalHelper.SymbolKeyHash {
		t.Fatalf("both distinct Helper symbols were not persisted: production=%+v external=%+v", persistedProductionHelper, persistedExternalHelper)
	}

	persistedRelations, err := ciStore.ListRelationsForSymbol(ctx, build.ID, persistedProductionHelper.ID)
	if err != nil {
		t.Fatalf("list relations for production Helper: %v", err)
	}
	var persistedExternalRelation bool
	for _, relation := range persistedRelations {
		if relation.FromSymbolKeyHash == externalTest.SymbolKeyHash && relation.ToSymbolKeyHash == persistedProductionHelper.SymbolKeyHash &&
			relation.ToSymbolID != nil && *relation.ToSymbolID == persistedProductionHelper.ID {
			persistedExternalRelation = true
		}
	}
	if !persistedExternalRelation {
		t.Fatal("persisted external-test relation does not point to production Helper")
	}
}

func TestCodeIndexBuild_IdempotencyAndJobCreation(t *testing.T) {
	_, jobsStore, ciStore, _ := setupCodeIntelTestDB(t)
	ctx := context.Background()

	snapID := "snap-idemp-001"
	modulePath := "github.com/example/ordersvc"
	bc := codeintelmodel.DefaultBuildContext()

	// 1. First build creation -> Created + AnalysisJob(PENDING)
	build1, created1, err := ciStore.GetOrCreateBuild(ctx, snapID, modulePath, bc)
	if err != nil || !created1 || build1 == nil {
		t.Fatalf("first build creation failed: %v", err)
	}
	if build1.Status != codeintelmodel.BuildStatusCreated {
		t.Errorf("expected CREATED status, got %s", build1.Status)
	}

	// Verify AnalysisJob created
	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeBuildCodeIndex, string(rune(build1.ID)))
	// Note: resource_id is fmt.Sprintf("%d", build1.ID)
	job, err = jobsStore.GetJobByID(ctx, 1)
	if err != nil || job == nil {
		t.Fatalf("expected analysis job for build 1, got err: %v", err)
	}
	if job.JobType != jobs.JobTypeBuildCodeIndex {
		t.Errorf("expected job type BUILD_CODE_INDEX, got %s", job.JobType)
	}

	// 2. Second request with same parameters -> Return existing record
	build2, created2, err := ciStore.GetOrCreateBuild(ctx, snapID, modulePath, bc)
	if err != nil {
		t.Fatalf("second build query failed: %v", err)
	}
	if created2 {
		t.Errorf("expected created=false for duplicate build request")
	}
	if build2.ID != build1.ID {
		t.Errorf("expected same build ID %d, got %d", build1.ID, build2.ID)
	}
}

func TestQueuedCodeIndexJobRestoresPersistedBuildTags(t *testing.T) {
	_, _, ciStore, snapStore := setupCodeIntelTestDB(t)
	ctx := context.Background()
	base := t.TempDir()
	storeFS := snapshotstore.NewLocalSnapshotStore(base)
	const repoID, snapshotID = "repo-build-tags", "snap-build-tags"
	sourceDir, err := storeFS.EnsureDir(repoID, snapshotID)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(sourceDir, "go.mod"), "module example.com/buildtags\n\ngo 1.22\n")
	writeTestFile(t, filepath.Join(sourceDir, "default.go"), "package buildtags\n\nfunc DefaultTarget() {}\n")
	writeTestFile(t, filepath.Join(sourceDir, "custom.go"), "//go:build custom\n\npackage buildtags\n\nfunc CustomTarget() {}\n")
	now := time.Now().UTC()
	if err := snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID: snapshotID, RepositoryID: repoID, CommitSHA: "commit-build-tags", Ref: "main",
		MaterializedPath: sourceDir, ContentHash: "build-tags-content", Status: snapshot.StatusReady, ReadyAt: &now,
	}); err != nil {
		t.Fatal(err)
	}
	bc := codeintelmodel.BuildContext{GOOS: "linux", GOARCH: "amd64", BuildTags: []string{"custom"}}
	build, created, err := ciStore.GetOrCreateBuild(ctx, snapshotID, "example.com/buildtags", bc)
	if err != nil || !created {
		t.Fatalf("create tagged build: created=%t err=%v", created, err)
	}
	if build.BuildTagsJSON != `["custom"]` {
		t.Fatalf("persisted build tags = %q, want [\"custom\"]", build.BuildTagsJSON)
	}
	handler := codeintel.NewCodeIndexJobHandler(ciStore, snapStore, storeFS, codeintel.NewAnalyzer())
	if err := handler.Execute(ctx, &jobs.AnalysisJob{ResourceID: strconv.FormatInt(build.ID, 10), MaxAttempts: 3}); err != nil {
		t.Fatalf("execute queued code-index job: %v", err)
	}
	ready, err := ciStore.GetByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ready.Status != codeintelmodel.BuildStatusReady || ready.BuildContextHash != bc.BuildContextHash() || ready.BuildTagsHash != bc.BuildTagsHash() {
		t.Fatalf("ready build lost its requested context: %+v", ready)
	}
	symbols, err := ciStore.ListAllSymbols(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	var foundCustom bool
	for _, symbol := range symbols {
		if symbol.Name == "CustomTarget" {
			foundCustom = true
		}
	}
	if !foundCustom {
		t.Fatalf("worker omitted custom-tagged symbol; persisted build context was not restored: %+v", symbols)
	}
}

func TestQueuedCodeIndexJobFailsClosedWhenLegacyTagNamesAreUnavailable(t *testing.T) {
	db, _, ciStore, snapStore := setupCodeIntelTestDB(t)
	ctx := context.Background()
	bc := codeintelmodel.BuildContext{GOOS: "linux", GOARCH: "amd64", BuildTags: []string{"custom"}}
	build, created, err := ciStore.GetOrCreateBuild(ctx, "snap-legacy-build-tags", "example.com/legacy", bc)
	if err != nil || !created {
		t.Fatalf("create custom-tag build: created=%t err=%v", created, err)
	}
	// Migration 012 backfills the absent legacy JSON as []; the persisted hash
	// still proves that custom tag names were originally present but lost.
	if err := db.Model(&codeintelmodel.CodeIndexBuild{}).Where("id = ?", build.ID).Update("build_tags_json", "[]").Error; err != nil {
		t.Fatal(err)
	}
	handler := codeintel.NewCodeIndexJobHandler(ciStore, snapStore, snapshotstore.NewLocalSnapshotStore(t.TempDir()), codeintel.NewAnalyzer())
	if err := handler.Execute(ctx, &jobs.AnalysisJob{ResourceID: strconv.FormatInt(build.ID, 10), MaxAttempts: 3}); err == nil {
		t.Fatal("legacy build with missing custom tag names unexpectedly executed")
	}
	retired, err := ciStore.GetByID(ctx, build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Status != codeintelmodel.BuildStatusFailed || retired.ErrorCode != "BUILD_TAGS_UNAVAILABLE" {
		t.Fatalf("legacy build was not failed closed: %+v", retired)
	}
}

func TestCodeIndexBuild_ExecutionAndQualityPersistence(t *testing.T) {
	_, jobsStore, ciStore, snapStore := setupCodeIntelTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tempDir := t.TempDir()
	storeFS := snapshotstore.NewLocalSnapshotStore(tempDir)
	repoID := "repo-exec-001"
	snapID := "snap-exec-001"

	sourceDir, err := storeFS.EnsureDir(repoID, snapID)
	if err != nil {
		t.Fatalf("ensure dir failed: %v", err)
	}

	// Create sample Go files in snapshot
	goMod := filepath.Join(sourceDir, "go.mod")
	_ = os.WriteFile(goMod, []byte("module example.com/ordersvc\n\ngo 1.22\n"), 0644)

	mainGo := filepath.Join(sourceDir, "main.go")
	_ = os.WriteFile(mainGo, []byte(`package main

import "fmt"

type OrderProcessor struct {
	Name string
}

func (p *OrderProcessor) ProcessOrder(id string) error {
	fmt.Println("Processing order", id)
	return nil
}
`), 0644)

	mainTestGo := filepath.Join(sourceDir, "main_test.go")
	_ = os.WriteFile(mainTestGo, []byte(`package main

import "testing"

func TestProcessOrder(t *testing.T) {
	p := &OrderProcessor{Name: "test"}
	_ = p.ProcessOrder("order-1")
}
`), 0644)

	// Create Snapshot in DB
	now := time.Now().UTC()
	_ = snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID:               snapID,
		RepositoryID:     repoID,
		CommitSHA:        "commit-exec-001",
		Ref:              "main",
		MaterializedPath: sourceDir,
		Status:           snapshot.StatusReady,
		ReadyAt:          &now,
	})

	// Create CodeIndexBuild
	build, _, err := ciStore.GetOrCreateBuild(ctx, snapID, "example.com/ordersvc", codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("create build failed: %v", err)
	}

	// Setup Worker & CodeIndexJobHandler
	analyzer := codeintel.NewAnalyzer()
	handler := codeintel.NewCodeIndexJobHandler(ciStore, snapStore, storeFS, analyzer)

	workerCfg := jobs.DefaultWorkerConfig()
	workerCfg.PollInterval = 20 * time.Millisecond
	worker := jobs.NewWorker(jobsStore, workerCfg)
	worker.RegisterHandler(jobs.JobTypeBuildCodeIndex, handler)

	worker.Start(ctx)
	defer worker.Stop()

	// Poll until build reaches READY
	deadline := time.Now().Add(5 * time.Second)
	var finalBuild *codeintelmodel.CodeIndexBuild
	for time.Now().Before(deadline) {
		b, err := ciStore.GetByID(ctx, build.ID)
		if err == nil && b.Status == codeintelmodel.BuildStatusReady {
			finalBuild = b
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalBuild == nil {
		t.Fatalf("build did not reach READY within timeout")
	}

	if finalBuild.SymbolCount == 0 {
		t.Errorf("expected extracted symbols > 0, got %d", finalBuild.SymbolCount)
	}

	// Verify symbols query
	symbols, err := ciStore.ListSymbols(ctx, build.ID, "ProcessOrder", 10)
	if err != nil || len(symbols) == 0 {
		t.Fatalf("expected ProcessOrder symbol found in DB, got err=%v, count=%d", err, len(symbols))
	}
	if symbols[0].ReceiverCanonical != "OrderProcessor" {
		t.Errorf("expected canonical receiver OrderProcessor, got %s", symbols[0].ReceiverCanonical)
	}

	// Verify derived RetrievalBuild auto-created
	retrievalBuild, err := ciStore.GetRetrievalBuildByCodeIndexBuild(ctx, build.ID)
	if err != nil || retrievalBuild == nil {
		t.Fatalf("expected derived RetrievalBuild created, got err: %v", err)
	}
	if retrievalBuild.Strategy != "BM25" {
		t.Errorf("expected strategy BM25, got %s", retrievalBuild.Strategy)
	}
}

func TestLineageInvariantValidation(t *testing.T) {
	_, _, ciStore, snapStore := setupCodeIntelTestDB(t)
	ctx := context.Background()

	repoA := "repo-lineage-A"
	snapA := "snap-lineage-A"
	now := time.Now().UTC()

	_ = snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID:           snapA,
		RepositoryID: repoA,
		CommitSHA:    "sha-A",
		Ref:          "main",
		Status:       snapshot.StatusReady,
		ReadyAt:      &now,
	})

	buildA, _, err := ciStore.GetOrCreateBuild(ctx, snapA, "module-A", codeintelmodel.DefaultBuildContext())
	if err != nil {
		t.Fatalf("create build A failed: %v", err)
	}

	retBuildA, _, err := ciStore.GetOrCreateRetrievalBuild(ctx, buildA.ID, "BM25")
	if err != nil {
		t.Fatalf("create retrieval build A failed: %v", err)
	}

	// 1. Valid lineage -> OK
	err = ciStore.ValidateLineage(ctx, repoA, snapA, buildA.ID, retBuildA.ID)
	if err != nil {
		t.Errorf("valid lineage failed validation: %v", err)
	}

	// 2. Mismatched snapshot ID -> Fail
	err = ciStore.ValidateLineage(ctx, repoA, "wrong-snapshot-id", buildA.ID, retBuildA.ID)
	if err == nil {
		t.Errorf("expected error on mismatched snapshot ID")
	}

	// 3. Mismatched retrieval build ID (different build chain) -> Fail
	err = ciStore.ValidateLineage(ctx, repoA, snapA, buildA.ID+999, retBuildA.ID)
	if err == nil {
		t.Errorf("expected error on mismatched code index build ID")
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write fixture %s: %v", path, err)
	}
}
