package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"repolens/internal/agent"
	"repolens/internal/codeintel"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/indexing"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	platformmysql "repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/retrieval"
	"repolens/internal/revision"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
	"repolens/internal/worker"
)

const goldenPathCommit = "0123456789012345678901234567890123456789"

func TestGoldenPathRevisionDiagnosisReport(t *testing.T) {
	db, jobStore, cleanup := setupMySQL(t)
	if db == nil {
		return
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	repositoryStore := repo.NewStore(db)
	snapshotStore := snapshot.NewStore(db)
	revisionStore := revision.NewStore(db)
	codeIndexStore := codeintelstore.NewStore(db)
	diagnosisStore := diagnosis.NewStore(db)
	reportStore := evidence.NewReportStore(db)
	citationStore := evidence.NewCitationStore(db)
	snapshotBasePath := t.TempDir()
	storeFS := snapshotstore.NewLocalSnapshotStore(snapshotBasePath)
	artifactDir := t.TempDir()

	repository := &repo.Repository{
		ID:         "repo-golden-path",
		UserID:     "user-golden-path",
		Name:       "example.com/checkout",
		GitURL:     "https://github.com/example/checkout",
		DefaultRef: "main",
		Status:     repo.StatusActive,
	}
	if err := repositoryStore.Create(ctx, repository); err != nil {
		t.Fatal(err)
	}

	revisionService := revision.NewService(
		revisionStore,
		repositoryStore,
		fixedResolver{},
		snapshotBasePath,
	)
	prepared, created, err := revisionService.Prepare(ctx, repository.UserID, repository.ID, "main")
	if err != nil || !created {
		t.Fatalf("prepare revision = created=%v err=%v", created, err)
	}

	workerCfg := jobs.DefaultWorkerConfig()
	workerCfg.WorkerID = "golden-path-worker"
	workerCfg.Concurrency = 1
	workerCfg.BatchSize = 1
	workerCfg.PollInterval = 20 * time.Millisecond
	workerCfg.ReapInterval = time.Second
	jobWorker := jobs.NewWorker(jobStore, workerCfg)

	snapshotHandler := indexing.NewSnapshotJobHandler(
		repositoryStore,
		snapshotStore,
		nil,
		storeFS,
		fixtureCloner{},
		indexing.NewFileFilter(512),
		indexing.NewCodeChunker(60, 10),
		nil,
	).WithRevisionStore(revisionStore)
	codeIndexHandler := codeintel.NewCodeIndexJobHandler(
		codeIndexStore,
		snapshotStore,
		storeFS,
		codeintel.NewAnalyzer(),
	).WithRevisionStore(revisionStore)
	retrievalHandler := retrieval.NewRetrievalJobHandler(codeIndexStore, artifactDir).WithRevisionStore(revisionStore)

	retriever := retrieval.NewProductionRetriever(codeIndexStore, artifactDir)
	provider := &scriptedProvider{}
	executor := agent.NewAgentRuntimeExecutor(
		provider,
		retriever,
		storeFS,
		trace.NewStore(db),
		agent.DefaultGuardConfig(),
	).WithCodeIntelStore(codeIndexStore).WithEvidencePacketLimit(32 * 1024).WithEvidenceIssuer(evidence.NewEvidenceIssuer(db, storeFS))
	diagnosisHandler := worker.NewDiagnosisJobHandler(
		diagnosisStore,
		reportStore,
		citationStore,
		evidence.NewCitationValidator(storeFS),
		executor,
	)

	jobWorker.RegisterHandler(jobs.JobTypeMaterializeSnapshot, snapshotHandler)
	jobWorker.RegisterHandler(jobs.JobTypeBuildCodeIndex, codeIndexHandler)
	jobWorker.RegisterHandler(jobs.JobTypeBuildRetrieval, retrievalHandler)
	jobWorker.RegisterHandler(jobs.JobTypeRunDiagnosis, diagnosisHandler)
	jobWorker.Start(ctx)
	defer jobWorker.Stop()

	readyRevision := waitForRevision(t, ctx, revisionStore, prepared.ID)
	if readyRevision.Status != revision.StatusReady || readyRevision.Stage != revision.StageReady {
		t.Fatalf("revision = %+v, want READY", readyRevision)
	}

	diagnosisService := diagnosis.NewService(diagnosisStore, repositoryStore, snapshotStore).
		WithCodeIntelStore(codeIndexStore).
		WithRevisionStore(revisionStore).
		WithProviderMetadata(diagnosis.ProviderMetadata{
			IsConfigured:    true,
			ModelName:       "scripted-golden-path",
			AgentConfigHash: "golden-path-config",
			Temperature:     0.1,
		})
	run, created, err := diagnosisService.Create(ctx, diagnosis.CreateDiagnosisInput{
		UserID:             repository.UserID,
		RepositoryID:       repository.ID,
		AnalysisRevisionID: readyRevision.ID,
		IssueTitle:         "Checkout processing returns an unexpected result",
		IssueDescription:   "The checkout handler does not preserve the expected processing behavior.",
		ErrorLog:           "checkout processing returned an unexpected result",
		IdempotencyKey:     "golden-path-diagnosis-1",
	})
	if err != nil || !created {
		t.Fatalf("create diagnosis = created=%v err=%v", created, err)
	}

	finalRun := waitForDiagnosis(t, ctx, diagnosisStore, run.ID)
	if finalRun.Status != diagnosis.StatusSucceeded {
		t.Fatalf("diagnosis = %+v, want SUCCEEDED", finalRun)
	}
	report, err := reportStore.GetByRunID(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if report.ReportStatus != evidence.ReportValid {
		t.Fatalf("report status = %s, want VALID", report.ReportStatus)
	}
	citations, err := citationStore.ListByReportID(ctx, report.ID)
	if err != nil || len(citations) != 1 || citations[0].ValidationStatus != evidence.CitationValid {
		t.Fatalf("citations = %+v err=%v, want one VALID citation", citations, err)
	}
	if atomic.LoadInt32(&provider.calls) == 0 {
		t.Fatal("scripted provider was not called")
	}
}

func setupMySQL(t *testing.T) (*gorm.DB, *jobs.Store, func()) {
	t.Helper()
	ctx := context.Background()
	container, err := tcmysql.RunContainer(
		ctx,
		testcontainers.WithImage("mysql:8.0"),
		tcmysql.WithDatabase("repolens_e2e"),
		tcmysql.WithUsername("e2e_user"),
		tcmysql.WithPassword("e2e_password"),
	)
	if err != nil {
		if os.Getenv("REPOLENS_REQUIRE_REAL_INTEGRATION") == "1" {
			t.Fatalf("real MySQL Golden Path is required but Docker/MySQL could not start: %v", err)
		}
		t.Skipf("skipping real MySQL Golden Path (Docker not available: %v)", err)
		return nil, nil, nil
	}

	connectionString, err := container.ConnectionString(ctx, "charset=utf8mb4&parseTime=True&loc=Local")
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatal(err)
	}
	db, err := gorm.Open(mysql.Open(connectionString), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		_ = container.Terminate(ctx)
		t.Fatal(err)
	}
	if err := platformmysql.ApplyMigrations(&platformmysql.DB{GormDB: db, SqlDB: sqlDB}, migrationPath()); err != nil {
		_ = container.Terminate(ctx)
		t.Fatal(err)
	}
	return db, jobs.NewStore(sqlDB), func() {
		_ = sqlDB.Close()
		_ = container.Terminate(context.Background())
	}
}

func migrationPath() string {
	return filepath.Join("..", "..", "migrations")
}

type fixedResolver struct{}

func (fixedResolver) ResolveRef(context.Context, string, string) (string, error) {
	return goldenPathCommit, nil
}

type fixtureCloner struct{}

func (fixtureCloner) ValidateGitURL(string) error { return nil }

func (fixtureCloner) CloneTo(ctx context.Context, _, _ string, targetDir string) (string, error) {
	return fixtureCloner{}.CloneCommitTo(ctx, "", goldenPathCommit, targetDir)
}

func (fixtureCloner) CloneCommitTo(_ context.Context, _, commitSHA, targetDir string) (string, error) {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", err
	}
	files := map[string]string{
		"go.mod":      "module example.com/checkout\n\ngo 1.22\n",
		"checkout.go": "package checkout\n\nfunc ProcessCheckout(id string) error {\n\treturn nil\n}\n",
	}
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(targetDir, name), []byte(contents), 0644); err != nil {
			return "", err
		}
	}
	return commitSHA, nil
}

type scriptedProvider struct {
	calls int32
}

func (p *scriptedProvider) Generate(_ context.Context, request llm.GenerateRequest) (llm.GenerateResponse, error) {
	atomic.AddInt32(&p.calls, 1)
	evidenceID := "ev_missing"
	for _, message := range request.Messages {
		var payload struct {
			EvidenceID string `json:"evidence_id"`
		}
		if json.Unmarshal([]byte(message.Content), &payload) == nil && payload.EvidenceID != "" {
			evidenceID = payload.EvidenceID
			break
		}
	}
	content := fmt.Sprintf(`{"conclusion_kind":"ROOT_CAUSE","summary":"The fixture checkout path returns without applying the expected processing behavior.","root_cause":"ProcessCheckout is the source location captured for the checkout behavior.","findings":[{"title":"Checkout processing implementation","reasoning":"The deterministic fixture provider identified the checkout implementation and attached a source citation.","citations":[{"evidence_id":%q,"reason":"checkout implementation"}]}],"recommended_checks":["Add a regression test for checkout processing"],"confirmed_facts":["The checkout implementation is present in the prepared snapshot."],"limitations":[],"confidence":0.9}`, evidenceID)
	return llm.GenerateResponse{
		Message: llm.Message{
			Role:    llm.RoleAssistant,
			Content: content,
		},
		FinishReason:     "stop",
		PromptTokens:     32,
		CompletionTokens: 64,
	}, nil
}

func waitForRevision(t *testing.T, ctx context.Context, store revision.Store, id string) *revision.AnalysisRevision {
	t.Helper()
	for {
		value, err := store.GetByID(ctx, id)
		if err == nil && value.Status != revision.StatusPreparing {
			if value.Status == revision.StatusFailed {
				t.Fatalf("revision failed: %+v", value)
			}
			return value
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for revision: %v", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func waitForDiagnosis(t *testing.T, ctx context.Context, store diagnosis.Store, id string) *diagnosis.DiagnosisRun {
	t.Helper()
	for {
		value, err := store.GetByID(ctx, id)
		if err == nil && value.Status != diagnosis.StatusQueued && value.Status != diagnosis.StatusRunning {
			return value
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for diagnosis: %v", ctx.Err())
		case <-time.After(25 * time.Millisecond):
		}
	}
}
