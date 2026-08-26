package integration_real

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/jobs"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/retrieval"
	"repolens/internal/retrieval/artifact"
	"repolens/internal/snapshot"
)

func TestRealMySQL_FullBuildAndRetrievalPipeline(t *testing.T) {
	if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "" {
		t.Skip("skipping real MySQL retrieval test (set REPOLENS_REQUIRE_REAL_INTEGRATION=1)")
	}

	db, jobsStore, cleanup := setupRealMySQL(t)
	if cleanup != nil {
		defer cleanup()
	}
	ctx := context.Background()

	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	ciStore := codeintelstore.NewStore(db)

	tempBase := t.TempDir()
	storeFS := snapshotstore.NewLocalSnapshotStore(tempBase)
	indexStorageDir := filepath.Join(tempBase, "indexes")

	// 1. Create Repo and Snapshot with real disk Go files
	repoID := "repo-real-pipeline"
	snapID := "snap-real-pipeline"

	_ = repoStore.Create(ctx, &repo.Repository{
		ID:         repoID,
		UserID:     "user-real",
		Name:       "checkout-svc",
		GitURL:     "https://github.com/example/checkout-svc",
		DefaultRef: "main",
		Status:     "ACTIVE",
	})

	sourceDir, err := storeFS.EnsureDir(repoID, snapID)
	if err != nil {
		t.Fatalf("failed ensuring dir: %v", err)
	}

	_ = os.WriteFile(filepath.Join(sourceDir, "go.mod"), []byte("module example.com/checkout\n\ngo 1.22\n"), 0644)
	_ = os.WriteFile(filepath.Join(sourceDir, "checkout.go"), []byte(`package checkout

type CheckoutService struct {
	StoreName string
}

func (s *CheckoutService) ProcessCheckout(cartID string) error {
	return nil
}
`), 0644)

	now := time.Now().UTC()
	_ = snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID:               snapID,
		RepositoryID:     repoID,
		CommitSHA:        "commit-real-12345",
		Ref:              "main",
		MaterializedPath: sourceDir,
		Status:           snapshot.StatusReady,
		ReadyAt:          &now,
	})

	// 2. Trigger CodeIndexBuild
	build, created, err := ciStore.GetOrCreateBuild(ctx, snapID, "example.com/checkout", codeintelmodel.DefaultBuildContext())
	if err != nil || !created {
		t.Fatalf("failed creating CodeIndexBuild: %v", err)
	}

	// 3. Worker with handlers for BUILD_CODE_INDEX and BUILD_RETRIEVAL
	workerCfg := jobs.DefaultWorkerConfig()
	workerCfg.PollInterval = 20 * time.Millisecond
	worker := jobs.NewWorker(jobsStore, workerCfg)

	codeIndexHandler := codeintel.NewCodeIndexJobHandler(ciStore, snapStore, storeFS, codeintel.NewAnalyzer())
	retrievalHandler := retrieval.NewRetrievalJobHandler(ciStore, indexStorageDir)

	worker.RegisterHandler(jobs.JobTypeBuildCodeIndex, codeIndexHandler)
	worker.RegisterHandler(jobs.JobTypeBuildRetrieval, retrievalHandler)

	workerCtx, workerCancel := context.WithCancel(context.Background())
	defer workerCancel()
	worker.Start(workerCtx)
	defer worker.Stop()

	// 4. Poll until RetrievalBuild reaches READY
	deadline := time.Now().Add(10 * time.Second)
	var finalRB *codeintelmodel.RetrievalBuild
	for time.Now().Before(deadline) {
		rb, err := ciStore.GetRetrievalBuildByCodeIndexBuild(ctx, build.ID)
		if err == nil && rb != nil && rb.Status == codeintelmodel.BuildStatusReady {
			finalRB = rb
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if finalRB == nil {
		t.Fatalf("retrieval build did not reach READY within timeout")
	}

	// 5. Verify published artifact on disk
	idx, err := artifact.LoadIndex(finalRB.ArtifactPath)
	if err != nil || idx == nil {
		t.Fatalf("failed loading published BM25 index: %v", err)
	}
	if idx.TotalDocs == 0 {
		t.Errorf("expected non-empty indexed documents")
	}

	// 6. Test ProductionRetriever query against real published artifact
	retriever := retrieval.NewProductionRetriever(ciStore, indexStorageDir)
	results, err := retriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: snapID,
		Query:      "ProcessCheckout",
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("production search query failed: %v", err)
	}
	if len(results) == 0 {
		t.Fatalf("expected search results for ProcessCheckout")
	}
	if results[0].Symbol != "ProcessCheckout" {
		t.Errorf("expected top symbol ProcessCheckout, got %s", results[0].Symbol)
	}
}
