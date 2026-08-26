package integration_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/agent"
	codeintelmodel "repolens/internal/codeintel/model"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/jobs"
	"repolens/internal/platform/elasticsearch"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/retrieval"
	"repolens/internal/snapshot"
	"repolens/internal/worker"
)

func setupTestDB(t *testing.T) (*gorm.DB, *jobs.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "integration_test.db")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_busy_timeout=5000&_journal_mode=WAL"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test database: %v", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("failed to migrate test database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("failed getting sql.DB: %v", err)
	}
	jobsStore := jobs.NewStoreWithDriver(sqlDB, "sqlite3")
	return db, jobsStore
}

// 1. Test Diagnosis Creation & Idempotency
func TestDiagnosisCreationAndIdempotency(t *testing.T) {
	db, jobsStore := setupTestDB(t)
	ctx := context.Background()
	diagStore := diagnosis.NewStore(db)

	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	diagSvc := diagnosis.NewService(diagStore, repoStore, snapStore)

	// Seed Repo and Snapshot in READY status
	testRepo := &repo.Repository{
		ID:         "repo-100",
		UserID:     "user-100",
		Name:       "test-repo",
		GitURL:     "https://github.com/example/test-repo",
		DefaultRef: "main",
		Status:     "ACTIVE",
	}
	_ = repoStore.Create(ctx, testRepo)

	testSnap := &snapshot.RepositorySnapshot{
		ID:           "snap-100",
		RepositoryID: testRepo.ID,
		CommitSHA:    "abcdef123456",
		Ref:          "main",
		Status:       snapshot.StatusReady,
	}
	_ = snapStore.Create(ctx, testSnap)

	input := diagnosis.CreateDiagnosisInput{
		UserID:           "user-100",
		RepositoryID:     testRepo.ID,
		SnapshotID:       testSnap.ID,
		IssueTitle:       "Goroutine Leak Bug",
		IssueDescription: "Unbuffered channel write blocks indefinitely",
		ErrorLog:         "panic: deadlock",
		IdempotencyKey:   "idemp-key-001",
	}

	// 1. First submission -> Create Run & AnalysisJob
	run1, created1, err := diagSvc.Create(ctx, input)
	if err != nil || !created1 {
		t.Fatalf("first creation failed: %v", err)
	}
	if run1.Status != diagnosis.StatusQueued {
		t.Errorf("expected status QUEUED, got %s", run1.Status)
	}

	// Verification: Atomic AnalysisJob is created in PENDING status
	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run1.ID)
	if err != nil || job == nil {
		t.Fatalf("expected analysis_job created, got err: %v", err)
	}
	if job.Status != jobs.StatusPending {
		t.Errorf("expected job status PENDING, got %s", job.Status)
	}

	// 2. Duplicate submission -> Return existing run, is_duplicate=true
	run2, created2, err := diagSvc.Create(ctx, input)
	if err != nil {
		t.Fatalf("duplicate submission returned unexpected error: %v", err)
	}
	if created2 {
		t.Errorf("expected created=false for duplicate submission")
	}
	if run2.ID != run1.ID {
		t.Errorf("expected returned ID %s, got %s", run1.ID, run2.ID)
	}

	// 3. Payload mismatch with same idempotency key -> 409 Conflict
	conflictInput := input
	conflictInput.IssueTitle = "Different Issue Title"
	_, _, err = diagSvc.Create(ctx, conflictInput)
	if err != diagnosis.ErrIdempotencyConflict {
		t.Fatalf("expected ErrIdempotencyConflict, got %v", err)
	}
}

// 2. Test Concurrent Worker Claim Fencing via DB SKIP LOCKED
func TestConcurrentWorkerClaimFencing(t *testing.T) {
	db, jobsStore := setupTestDB(t)
	ctx := context.Background()

	diagStore := diagnosis.NewStore(db)
	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	diagSvc := diagnosis.NewService(diagStore, repoStore, snapStore)

	testRepo := &repo.Repository{ID: "repo-200", UserID: "u2", Name: "r2", GitURL: "https://github.com/a/b", Status: "ACTIVE"}
	_ = repoStore.Create(ctx, testRepo)
	testSnap := &snapshot.RepositorySnapshot{ID: "snap-200", RepositoryID: testRepo.ID, Status: snapshot.StatusReady}
	_ = snapStore.Create(ctx, testSnap)

	run, _, err := diagSvc.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID:         "u2",
		RepositoryID:   testRepo.ID,
		SnapshotID:     testSnap.ID,
		IssueTitle:     "Race Condition",
		IdempotencyKey: "key-race",
	})
	if err != nil {
		t.Fatalf("creation failed: %v", err)
	}

	// Worker A claims the job
	claimedA, err := jobsStore.ClaimJobs(ctx, "worker-A", 1, 30*time.Second)
	if err != nil || len(claimedA) != 1 {
		t.Fatalf("worker A claim failed: %v", err)
	}
	if claimedA[0].ResourceID != run.ID {
		t.Errorf("expected resource ID %s, got %s", run.ID, claimedA[0].ResourceID)
	}

	// Worker B attempts to claim at the same time -> Should find 0 claimable jobs
	claimedB, err := jobsStore.ClaimJobs(ctx, "worker-B", 1, 30*time.Second)
	if err != nil {
		t.Fatalf("worker B claim query failed: %v", err)
	}
	if len(claimedB) != 0 {
		t.Fatalf("expected Worker B to claim 0 jobs, got %d", len(claimedB))
	}
}

// 3. Test Full Worker Pipeline & Report Generation
func TestDBJobWorkerPipeline(t *testing.T) {
	db, jobsStore := setupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	diagSvc := diagnosis.NewService(diagStore, repoStore, snapStore)

	testRepo := &repo.Repository{ID: "repo-300", UserID: "u3", Name: "r3", GitURL: "https://github.com/a/b", Status: "ACTIVE"}
	_ = repoStore.Create(ctx, testRepo)
	testSnap := &snapshot.RepositorySnapshot{ID: "snap-300", RepositoryID: testRepo.ID, Status: snapshot.StatusReady}
	_ = snapStore.Create(ctx, testSnap)

	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	citVal := evidence.NewCitationValidator(storeFS)
	fakeExec := worker.NewFakeDiagnosisExecutor()

	handler := worker.NewDiagnosisJobHandler(
		diagStore,
		repStore,
		citStore,
		citVal,
		fakeExec,
	)

	workerCfg := jobs.DefaultWorkerConfig()
	workerCfg.PollInterval = 20 * time.Millisecond
	workerCfg.LeaseDuration = 10 * time.Second
	jobsWorker := jobs.NewWorker(jobsStore, workerCfg)
	jobsWorker.RegisterHandler(jobs.JobTypeRunDiagnosis, handler)

	jobsWorker.Start(ctx)
	defer jobsWorker.Stop()

	run, _, err := diagSvc.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID:         "u3",
		RepositoryID:   testRepo.ID,
		SnapshotID:     testSnap.ID,
		IssueTitle:     "Memory Leak Bug",
		IdempotencyKey: "k-pipeline",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Poll until completed
	deadline := time.Now().Add(5 * time.Second)
	var finalRun *diagnosis.DiagnosisRun
	for time.Now().Before(deadline) {
		r, err := diagStore.GetByID(ctx, run.ID)
		if err == nil && r.Status == diagnosis.StatusSucceeded {
			finalRun = r
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalRun == nil {
		t.Fatalf("diagnosis job did not complete within timeout")
	}

	rep, err := repStore.GetByRunID(ctx, run.ID)
	if err != nil || rep == nil {
		t.Fatalf("expected diagnosis report created, got: %v", err)
	}
	if rep.RootCause == "" {
		t.Errorf("expected non-empty root cause")
	}

	finalJob, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeRunDiagnosis, run.ID)
	if err != nil || finalJob.Status != jobs.StatusSucceeded {
		t.Errorf("expected analysis job SUCCEEDED, got %v", finalJob)
	}
}

// 4. Test Elasticsearch & RRF Integration
func TestElasticsearchAndRRFIntegration(t *testing.T) {
	// Mock Elasticsearch Server
	mockES := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"version":{"number":"8.13.0"}}`))
			return
		}

		if r.URL.Path == "/test_index/_search" {
			mockResp := `{
				"hits": {
					"total": {"value": 2, "relation": "eq"},
					"hits": [
						{
							"_id": "chunk-1",
							"_score": 4.5,
							"_source": {
								"snapshot_id": "snap-mock",
								"path": "internal/auth/jwt.go",
								"start_line": 10,
								"end_line": 35,
								"content": "func GenerateToken(userID string) (string, error) {",
								"content_hash": "hash1"
							}
						},
						{
							"_id": "chunk-2",
							"_score": 3.2,
							"_source": {
								"snapshot_id": "snap-mock",
								"path": "internal/user/service.go",
								"start_line": 50,
								"end_line": 70,
								"content": "func Login(username, password string) (*User, error) {",
								"content_hash": "hash2"
							}
						}
					]
				}
			}`
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(mockResp))
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"acknowledged": true}`))
	}))
	defer mockES.Close()

	esClient := elasticsearch.NewClient(mockES.URL, "test_index")
	bm25Retriever := retrieval.NewESBM25Retriever(esClient)
	embedder := retrieval.NewLocalTFIDFEmbeddingProvider(128)
	vectorRetriever := retrieval.NewESVectorRetriever(esClient, embedder)

	rrfRetriever := retrieval.NewHybridRRFRetriever(60, bm25Retriever, vectorRetriever)

	ctx := context.Background()
	results, err := rrfRetriever.Search(ctx, retrieval.SearchRequest{
		SnapshotID: "snap-mock",
		Query:      "login authentication",
		TopK:       5,
	})
	if err != nil {
		t.Fatalf("RRF retrieval failed: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("expected non-empty search results from mock ES")
	}

	if results[0].Path != "internal/auth/jwt.go" {
		t.Errorf("expected top result internal/auth/jwt.go, got %s", results[0].Path)
	}
}

// 5. Test Application Retry on 429 Rate Limit
type FlakyRateLimitExecutor struct {
	attemptCount int
}

func (e *FlakyRateLimitExecutor) Execute(ctx context.Context, run *diagnosis.DiagnosisRun, attempt *diagnosis.DiagnosisAttempt) (*agent.ExecutionResult, error) {
	e.attemptCount++
	if e.attemptCount == 1 {
		return &agent.ExecutionResult{
			Retryable:    true,
			ErrorCode:    "RATE_LIMIT",
			ErrorMessage: "rate limit 429 from LLM API",
		}, fmt.Errorf("rate limit 429 from LLM API")
	}

	report := &evidence.DiagnosisReportData{
		Summary:   "Fixed after rate limit backoff",
		RootCause: "Rate limit resolved",
		Findings:  []evidence.Finding{},
	}
	return &agent.ExecutionResult{
		Report:           report,
		PromptTokens:     100,
		CompletionTokens: 200,
		ToolCalls:        1,
		Retryable:        false,
	}, nil
}

func TestApplicationRetryOn429RateLimit(t *testing.T) {
	db, jobsStore := setupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	diagStore := diagnosis.NewStore(db)
	repStore := evidence.NewReportStore(db)
	citStore := evidence.NewCitationStore(db)
	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	diagSvc := diagnosis.NewService(diagStore, repoStore, snapStore)

	testRepo := &repo.Repository{ID: "repo-400", UserID: "u4", Name: "r4", GitURL: "https://github.com/a/b", Status: "ACTIVE"}
	_ = repoStore.Create(ctx, testRepo)
	testSnap := &snapshot.RepositorySnapshot{ID: "snap-400", RepositoryID: testRepo.ID, Status: snapshot.StatusReady}
	_ = snapStore.Create(ctx, testSnap)

	flakyExec := &FlakyRateLimitExecutor{}
	storeFS := snapshotstore.NewLocalSnapshotStore(t.TempDir())
	citVal := evidence.NewCitationValidator(storeFS)

	handler := worker.NewDiagnosisJobHandler(
		diagStore,
		repStore,
		citStore,
		citVal,
		flakyExec,
	)

	workerCfg := jobs.DefaultWorkerConfig()
	workerCfg.PollInterval = 20 * time.Millisecond
	workerCfg.BaseBackoff = 20 * time.Millisecond
	workerCfg.MaxBackoff = 50 * time.Millisecond

	jobsWorker := jobs.NewWorker(jobsStore, workerCfg)
	jobsWorker.RegisterHandler(jobs.JobTypeRunDiagnosis, handler)

	jobsWorker.Start(ctx)
	defer jobsWorker.Stop()

	run, _, err := diagSvc.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID:         "u4",
		RepositoryID:   testRepo.ID,
		SnapshotID:     testSnap.ID,
		IssueTitle:     "Flaky LLM Test",
		IdempotencyKey: "k-flaky",
	})
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Poll until completed (should retry once and succeed on 2nd attempt)
	deadline := time.Now().Add(5 * time.Second)
	var finalRun *diagnosis.DiagnosisRun
	for time.Now().Before(deadline) {
		r, err := diagStore.GetByID(ctx, run.ID)
		if err == nil && r.Status == diagnosis.StatusSucceeded {
			finalRun = r
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if finalRun == nil {
		t.Fatalf("diagnosis run did not complete with SUCCEEDED after retry")
	}

	if flakyExec.attemptCount != 2 {
		t.Errorf("expected exactly 2 attempts (1 failure + 1 success), got %d", flakyExec.attemptCount)
	}
}

// 6. Test Milestone 4: Snapshot Materialization & Code Index Chaining
func TestMilestone4_SnapshotAndCodeIndexChaining(t *testing.T) {
	db, jobsStore := setupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	repoStore := repo.NewStore(db)
	snapStore := snapshot.NewStore(db)
	ciStore := codeintelstore.NewStore(db)

	testRepo := &repo.Repository{ID: "repo-m4", UserID: "u-m4", Name: "payment-svc", GitURL: "https://github.com/a/b", Status: "ACTIVE"}
	_ = repoStore.Create(ctx, testRepo)

	now := time.Now().UTC()
	testSnap := &snapshot.RepositorySnapshot{
		ID:               "snap-m4",
		RepositoryID:     testRepo.ID,
		CommitSHA:        "sha-m4-12345",
		Ref:              "main",
		MaterializedPath: "/tmp/fake/path",
		Status:           snapshot.StatusReady,
		ReadyAt:          &now,
	}
	_ = snapStore.Create(ctx, testSnap)

	// Create CodeIndexBuild + AnalysisJob
	build, created, err := ciStore.GetOrCreateBuild(ctx, testSnap.ID, testRepo.Name, codeintelmodel.DefaultBuildContext())
	if err != nil || !created {
		t.Fatalf("failed creating CodeIndexBuild: %v", err)
	}

	job, err := jobsStore.GetJobByResource(ctx, jobs.JobTypeBuildCodeIndex, fmt.Sprintf("%d", build.ID))
	if err != nil || job == nil {
		t.Fatalf("expected BUILD_CODE_INDEX job created, got %v", err)
	}
	if job.Status != jobs.StatusPending {
		t.Errorf("expected job status PENDING, got %s", job.Status)
	}
}

// 7. Test Milestone 4: Lineage Invariant Prevention
func TestMilestone4_LineageMismatchPrevention(t *testing.T) {
	db, _ := setupTestDB(t)
	ctx := context.Background()

	ciStore := codeintelstore.NewStore(db)
	snapStore := snapshot.NewStore(db)

	now := time.Now().UTC()
	_ = snapStore.Create(ctx, &snapshot.RepositorySnapshot{
		ID:           "snap-correct",
		RepositoryID: "repo-correct",
		CommitSHA:    "sha-1",
		Ref:          "main",
		Status:       snapshot.StatusReady,
		ReadyAt:      &now,
	})

	build, _, _ := ciStore.GetOrCreateBuild(ctx, "snap-correct", "mod", codeintelmodel.DefaultBuildContext())
	retBuild, _, _ := ciStore.GetOrCreateRetrievalBuild(ctx, build.ID, "BM25")

	// Same lineage -> Pass
	err := ciStore.ValidateLineage(ctx, "repo-correct", "snap-correct", build.ID, retBuild.ID)
	if err != nil {
		t.Errorf("expected valid lineage, got: %v", err)
	}

	// Mismatched snapshot -> Fail with BUILD_LINEAGE_MISMATCH
	errMismatch := ciStore.ValidateLineage(ctx, "repo-correct", "snap-alien", build.ID, retBuild.ID)
	if errMismatch == nil {
		t.Errorf("expected lineage mismatch error for alien snapshot")
	}
}
