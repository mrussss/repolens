package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"repolens/internal/agent"
	"repolens/internal/codeintel"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/indexing"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	"repolens/internal/platform/config"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/retrieval"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
	"repolens/internal/worker"
)

func main() {
	if err := run(); err != nil {
		logger.L(context.Background()).Error("worker daemon fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	logger.Init(cfg.Env)
	log := logger.L(context.Background())

	log.Info("starting RepoLens DB-backed Analysis Job Worker daemon", "env", cfg.Env)

	db, err := mysql.Connect(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	if err := mysql.AutoMigrate(db.GormDB); err != nil {
		return fmt.Errorf("failed to run database auto migrations: %w", err)
	}

	storeFS := snapshotstore.NewLocalSnapshotStore(cfg.SnapshotBasePath)
	repoStore := repo.NewStore(db.GormDB)
	snapshotStore := snapshot.NewStore(db.GormDB)
	indexStore := repoindex.NewStore(db.GormDB)
	codeIntelStore := codeintelstore.NewStore(db.GormDB)
	diagnosisStore := diagnosis.NewStore(db.GormDB)
	reportStore := evidence.NewReportStore(db.GormDB)
	citationStore := evidence.NewCitationStore(db.GormDB)
	traceStore := trace.NewStore(db.GormDB)
	citationVal := evidence.NewCitationValidator(storeFS)

	// LLM Provider Validation
	var provider llm.Provider
	switch cfg.LLMProvider {
	case "openai":
		if cfg.LLMAPIKey == "" {
			if cfg.Env == "production" {
				return fmt.Errorf("LLM_API_KEY is required for openai LLM provider in production mode")
			}
			log.Warn("LLM_API_KEY is empty for openai provider, falling back to fake provider in non-production mode")
			provider = llm.NewFakeProvider(llm.ModeNormalStructured)
		} else {
			provider = llm.NewOpenAICompatibleProvider(cfg.LLMAPIKey, cfg.LLMBaseURL, cfg.LLMModel)
		}
	case "fake":
		if cfg.Env == "production" {
			return fmt.Errorf("LLM_PROVIDER=fake is strictly forbidden in production mode")
		}
		log.Warn("using fake LLM provider for development / testing mode")
		provider = llm.NewFakeProvider(llm.ModeNormalStructured)
	default:
		return fmt.Errorf("unsupported LLM_PROVIDER: %q (supported: 'openai', 'fake')", cfg.LLMProvider)
	}

	// Pure Go Production Retrieval (BM25 + Structural Code Intelligence)
	indexStorageDir := filepath.Join(cfg.SnapshotBasePath, "indexes")
	activeRetriever := retrieval.NewProductionRetriever(codeIntelStore, indexStorageDir)

	cloner := indexing.NewSafeGitCloner(cfg.AllowHosts, cfg.MaxRepoSizeMB, 2*time.Minute)
	filter := indexing.NewFileFilter(cfg.MaxFileSizeKB)
	chunker := indexing.NewCodeChunker(60, 10)

	agentExecutor := agent.NewAgentRuntimeExecutor(
		provider,
		activeRetriever,
		storeFS,
		traceStore,
		agent.DefaultGuardConfig(),
	).WithCodeIntelStore(codeIntelStore)

	// DB-backed Analysis Job Worker Runtime
	jobsStore := jobs.NewStoreWithDriver(db.SqlDB, cfg.DBDriver)
	diagJobHandler := worker.NewDiagnosisJobHandler(
		diagnosisStore,
		reportStore,
		citationStore,
		citationVal,
		agentExecutor,
	)
	snapshotJobHandler := indexing.NewSnapshotJobHandler(
		repoStore,
		snapshotStore,
		indexStore,
		storeFS,
		cloner,
		filter,
		chunker,
		nil,
	).WithCodeIntelStore(codeIntelStore)

	codeIndexJobHandler := codeintel.NewCodeIndexJobHandler(
		codeIntelStore,
		snapshotStore,
		storeFS,
		codeintel.NewAnalyzer(),
	)

	retrievalJobHandler := retrieval.NewRetrievalJobHandler(
		codeIntelStore,
		indexStorageDir,
	)

	jobsWorker := jobs.NewWorker(jobsStore, jobs.DefaultWorkerConfig())
	jobsWorker.RegisterHandler(jobs.JobTypeRunDiagnosis, diagJobHandler)
	jobsWorker.RegisterHandler(jobs.JobTypeMaterializeSnapshot, snapshotJobHandler)
	jobsWorker.RegisterHandler(jobs.JobTypeBuildCodeIndex, codeIndexJobHandler)
	jobsWorker.RegisterHandler(jobs.JobTypeBuildRetrieval, retrievalJobHandler)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	jobsWorker.Start(ctx)
	log.Info("analysis jobs worker started successfully with all 4 job handlers: RUN_DIAGNOSIS, MATERIALIZE_SNAPSHOT, BUILD_CODE_INDEX, BUILD_RETRIEVAL")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	sig := <-sigChan
	log.Info("received shutdown signal", "signal", sig.String())

	cancel()
	log.Info("shutting down worker daemon, draining in-flight jobs...")
	jobsWorker.Stop()
	log.Info("analysis jobs worker shut down cleanly")

	return nil
}
