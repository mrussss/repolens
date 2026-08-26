package codeintel_test

import (
	"context"
	"os"
	"path/filepath"
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
