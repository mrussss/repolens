package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"repolens/internal/agent"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/indexing"
	"repolens/internal/llm"
	"repolens/internal/mq"
	"repolens/internal/platform/config"
	"repolens/internal/platform/elasticsearch"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repoindex"
	"repolens/internal/retrieval"
	"repolens/internal/snapshot"
	"repolens/internal/sse"
	"repolens/internal/trace"
	"repolens/internal/worker"
)

func main() {
	cfg := config.Load()
	logger.Init(cfg.Env)
	log := logger.L(context.Background())

	log.Info("starting RepoLens Worker daemon", "env", cfg.Env)

	db, err := mysql.Connect(cfg)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		return
	}
	defer db.Close()

	var broker mq.Broker
	rmqBroker, err := mq.NewRabbitMQBroker(cfg.RabbitMQURL)
	if err != nil {
		if cfg.Env == "production" {
			log.Error("failed to connect to rabbitmq in production mode", "error", err)
			return
		}
		log.Warn("failed to connect to rabbitmq, using memory broker", "error", err)
		broker = mq.NewMemoryBroker()
	} else {
		broker = rmqBroker
		defer broker.Close()
	}

	storeFS := snapshotstore.NewLocalSnapshotStore(cfg.SnapshotBasePath)
	snapshotStore := snapshot.NewStore(db.GormDB)
	indexStore := repoindex.NewStore(db.GormDB)
	diagnosisStore := diagnosis.NewStore(db.GormDB)
	reportStore := evidence.NewReportStore(db.GormDB)
	citationStore := evidence.NewCitationStore(db.GormDB)
	traceStore := trace.NewStore(db.GormDB)
	citationVal := evidence.NewCitationValidator(storeFS)

	// Retrieval engines
	var embedder retrieval.EmbeddingProvider
	hasNeuralEmbedding := cfg.EmbeddingProvider == "openai" && cfg.EmbeddingAPIKey != ""
	if hasNeuralEmbedding {
		embedder = retrieval.NewOpenAICompatibleEmbeddingProvider(cfg.EmbeddingAPIKey, cfg.EmbeddingBaseURL, cfg.EmbeddingModel, cfg.EmbeddingDim)
	} else {
		embedder = retrieval.NewLocalTFIDFEmbeddingProvider(cfg.EmbeddingDim)
	}

	chunkStore := retrieval.NewMemoryChunkStore()
	var indexWriter indexing.ChunkIndexWriter = chunkStore
	var bm25Retriever retrieval.Retriever = retrieval.NewBM25Retriever(chunkStore)
	var vectorRetriever retrieval.Retriever = retrieval.NewVectorRetriever(chunkStore, embedder)

	if cfg.ESURL != "" {
		esClient := elasticsearch.NewClient(cfg.ESURL, cfg.ESIndexName)
		if err := esClient.Ping(context.Background()); err == nil {
			_ = esClient.EnsureIndex(context.Background(), embedder.Dimension())
			bm25Retriever = retrieval.NewESBM25Retriever(esClient)
			vectorRetriever = retrieval.NewESVectorRetriever(esClient, embedder)
			esWriter := retrieval.NewElasticsearchChunkIndexWriter(esClient, embedder)
			indexWriter = retrieval.NewCompositeChunkIndexWriter(chunkStore, esWriter)
			log.Info("connected to elasticsearch 8 cluster for retrieval and indexing", "url", cfg.ESURL, "index", cfg.ESIndexName)
		} else {
			if cfg.Env == "production" {
				log.Error("failed to connect to elasticsearch in production mode", "error", err)
				return
			}
			log.Warn("elasticsearch not available, falling back to in-memory retrieval", "error", err)
		}
	}

	var activeRetriever retrieval.Retriever
	switch cfg.RetrievalStrategy {
	case "hybrid":
		if !hasNeuralEmbedding {
			if cfg.Env == "production" {
				log.Error("hybrid retrieval strategy requested in production but no neural embedding API key configured")
				return
			}
			log.Warn("hybrid retrieval strategy requested without neural embedding API key, using local hashed baseline")
		}
		activeRetriever = retrieval.NewHybridRRFRetriever(60, bm25Retriever, vectorRetriever)
		log.Info("retrieval strategy configured", "strategy", "hybrid", "neural_embedding", hasNeuralEmbedding)
	case "bm25", "":
		activeRetriever = bm25Retriever
		log.Info("retrieval strategy configured", "strategy", "bm25")
	default:
		log.Warn("unknown retrieval strategy, defaulting to bm25", "strategy", cfg.RetrievalStrategy)
		activeRetriever = bm25Retriever
	}

	// Index Worker
	cloner := indexing.NewSafeGitCloner(cfg.AllowHosts, cfg.MaxRepoSizeMB, 2*time.Minute)
	filter := indexing.NewFileFilter(cfg.MaxFileSizeKB)
	chunker := indexing.NewCodeChunker(60, 10)
	indexWorker := indexing.NewIndexWorker(broker, snapshotStore, indexStore, storeFS, cloner, filter, chunker, indexWriter, 2)

	// LLM Provider
	var provider llm.Provider
	if cfg.LLMProvider == "openai" && cfg.LLMAPIKey != "" {
		provider = llm.NewOpenAICompatibleProvider(cfg.LLMAPIKey, cfg.LLMBaseURL, cfg.LLMModel)
	} else {
		provider = llm.NewFakeProvider(llm.ModeNormalStructured)
	}

	sseHub := sse.NewHub()
	agentExecutor := agent.NewAgentRuntimeExecutor(
		provider,
		activeRetriever,
		storeFS,
		traceStore,
		sseHub,
		agent.DefaultGuardConfig(),
	)

	// Diagnosis Consumer
	consumerCfg := worker.ConsumerConfig{
		Prefetch:        5,
		MaxAttempts:     3,
		AttemptDeadline: 5 * time.Minute,
		RetryBackoff:    5 * time.Second,
	}
	diagnosisConsumer := worker.NewDiagnosisConsumer(
		consumerCfg,
		broker,
		diagnosisStore,
		reportStore,
		citationStore,
		citationVal,
		agentExecutor,
		sseHub,
	)

	// Stale Attempt Recovery Sweeper
	recoverySweeper := worker.NewRecoverySweeper(diagnosisStore, 30*time.Second, 10*time.Second, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 2)

	go func() {
		if err := indexWorker.Start(ctx); err != nil && ctx.Err() == nil {
			log.Error("index worker daemon encountered fatal error", "error", err)
			errCh <- fmt.Errorf("index worker error: %w", err)
		}
	}()

	go func() {
		if err := diagnosisConsumer.Start(ctx); err != nil && ctx.Err() == nil {
			log.Error("diagnosis consumer daemon encountered fatal error", "error", err)
			errCh <- fmt.Errorf("diagnosis consumer error: %w", err)
		}
	}()

	go recoverySweeper.Start(ctx)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigChan:
		log.Info("received shutdown signal", "signal", sig.String())
	case daemonErr := <-errCh:
		log.Error("critical worker daemon component crashed, triggering immediate shutdown", "error", daemonErr)
	}

	cancel()
	log.Info("waiting for workers and in-flight tasks to exit cleanly...")
	time.Sleep(1 * time.Second)
	log.Info("RepoLens Worker daemon shutdown completed")
}
