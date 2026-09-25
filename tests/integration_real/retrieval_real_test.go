package integration_real

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"repolens/internal/codeintel"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
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
	longRelativeDir := filepath.Join("pkg", "nested")
	longFileName := strings.Repeat("f", 245) + ".go"
	longRelativePath := filepath.ToSlash(filepath.Join(longRelativeDir, longFileName))
	if len(longRelativePath) <= 255 || len(longRelativePath) > 512 {
		t.Fatalf("test path length = %d; want 256..512", len(longRelativePath))
	}
	longSourcePath := filepath.Join(sourceDir, filepath.FromSlash(longRelativePath))
	if err := os.MkdirAll(filepath.Dir(longSourcePath), 0755); err != nil {
		t.Fatalf("create long source directory: %v", err)
	}
	if err := os.WriteFile(longSourcePath, []byte(`package checkout

type CheckoutService struct {
	StoreName string
}

func (s *CheckoutService) ProcessCheckout(cartID string) error {
	return nil
}
`), 0644); err != nil {
		t.Fatalf("write long source file: %v", err)
	}

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
	retrievalHandler := retrieval.NewRetrievalJobHandler(ciStore, indexStorageDir).WithSnapshotSource(snapStore, storeFS)

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
		SnapshotID: snapID, CodeIndexBuildID: build.ID, RetrievalBuildID: finalRB.ID,
		Query: "ProcessCheckout", TopK: 5,
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
	if results[0].Path != longRelativePath {
		t.Fatalf("production result path = %q, want %q", results[0].Path, longRelativePath)
	}
	if len(results[0].ChunkID) <= 128 {
		t.Fatalf("production ChunkID length = %d, want >128: %q", len(results[0].ChunkID), results[0].ChunkID)
	}

	diagnosisStore := diagnosis.NewStore(db)
	run := &diagnosis.DiagnosisRun{
		ID: "run-real-long-chunk", UserID: "user-real", RepositoryID: repoID, SnapshotID: snapID,
		CodeIndexBuildID: build.ID, RetrievalBuildID: finalRB.ID, IssueTitle: "long retrieval chunk",
		ProviderEndpointFingerprint: "endpoint-fingerprint", ProviderConfigFingerprint: "config-fingerprint",
		NormalizedBaseURL: "https://api.example/v1", ModelName: "test-model", PromptVersion: "prompt-v1",
		AgentVersion: "agent-v1", AgentConfigHash: "agent-config", IdempotencyKey: "long-chunk-evidence",
		IdempotencyRequestHash: "long-chunk-evidence-request", Status: diagnosis.StatusQueued,
	}
	if err := db.WithContext(ctx).Create(run).Error; err != nil {
		t.Fatalf("create lineage diagnosis run: %v", err)
	}
	now = time.Now().UTC()
	attempt := &diagnosis.DiagnosisAttempt{
		ID: "attempt-real-long-chunk", DiagnosisRunID: run.ID, WorkerID: "worker-real-long-chunk",
		ExecutionGeneration: 1, AttemptNo: 1, StartedAt: now, HeartbeatAt: now, DeadlineAt: now.Add(time.Minute),
	}
	if err := diagnosisStore.StartAttempt(ctx, run.ID, attempt); err != nil {
		t.Fatalf("create lineage diagnosis attempt: %v", err)
	}
	issuer := evidence.NewEvidenceIssuer(db, storeFS)
	evidenceItem, err := issuer.Issue(ctx, evidence.IssueRequest{
		AttemptID: attempt.ID, DiagnosisRunID: run.ID,
		RepositoryID: repoID, SnapshotID: snapID, CodeIndexBuildID: build.ID,
		SourceKind: evidence.SourceInitialRetrieval, RetrievalChunkID: results[0].ChunkID,
		FilePath: results[0].Path, StartLine: results[0].StartLine, EndLine: results[0].EndLine,
	})
	if err != nil {
		t.Fatalf("issue evidence with long retrieval ChunkID: %v", err)
	}
	if evidenceItem.RetrievalChunkID != results[0].ChunkID || len(evidenceItem.RetrievalChunkID) <= 128 {
		t.Fatalf("persisted evidence ChunkID = %q (len=%d), want untruncated %q", evidenceItem.RetrievalChunkID, len(evidenceItem.RetrievalChunkID), results[0].ChunkID)
	}
	lineage := evidence.DraftLineage{
		AttemptID: attempt.ID, DiagnosisRunID: run.ID,
		RepositoryID: repoID, SnapshotID: snapID, CodeIndexBuildID: build.ID,
	}
	if err := issuer.Verify(ctx, evidenceItem, lineage); err != nil {
		t.Fatalf("verify long-path evidence lineage: %v", err)
	}

	storedReport := &evidence.Report{
		DiagnosisRunID: run.ID, AttemptID: attempt.ID, RootCause: "long-path retrieval evidence",
		FindingsJSON: "[]", RecommendedChecksJSON: "[]",
	}
	if err := evidence.NewReportStore(db).Create(ctx, storedReport); err != nil {
		t.Fatalf("persist report for long-path citation: %v", err)
	}
	citations := evidence.NewCitationStore(db)
	citation := evidence.Citation{
		ReportID: storedReport.ID, EvidenceID: evidenceItem.ID,
		SnapshotID: snapID, CodeIndexBuildID: build.ID, FilePath: longRelativePath,
		StartLine: evidenceItem.StartLine, EndLine: evidenceItem.EndLine,
		Excerpt: evidenceItem.DisplayExcerpt, Reason: "supports the retrieved code",
		ContentHash: evidenceItem.RawContentHash, ValidationStatus: evidence.CitationUnchecked,
	}
	evidence.NewCitationValidator(storeFS).Validate(ctx, repoID, snapID, &citation)
	if citation.ValidationStatus != evidence.CitationValid {
		t.Fatalf("long path citation failed production validation: %+v", citation)
	}
	if err := citations.CreateBatch(ctx, []evidence.Citation{citation}); err != nil {
		t.Fatalf("persist citation linked to long-path evidence: %v", err)
	}
	loadedCitations, err := citations.ListByReportID(ctx, storedReport.ID)
	if err != nil {
		t.Fatalf("read citation linked to long-path evidence: %v", err)
	}
	if len(loadedCitations) != 1 || loadedCitations[0].EvidenceID != evidenceItem.ID || loadedCitations[0].FilePath != longRelativePath || loadedCitations[0].SnapshotID != snapID || loadedCitations[0].CodeIndexBuildID != build.ID {
		t.Fatalf("citation/evidence lineage mismatch after persistence: %+v", loadedCitations)
	}
}
