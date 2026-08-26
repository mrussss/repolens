package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/platform/config"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/metrics"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
	"repolens/internal/trace"
)

func main() {
	if err := run(); err != nil {
		logger.L(context.Background()).Error("api server fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	logger.Init(cfg.Env)
	log := logger.L(context.Background())

	log.Info("starting RepoLens API server", "env", cfg.Env, "port", cfg.HTTPPort)

	db, err := mysql.Connect(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	if err := mysql.AutoMigrate(db.GormDB); err != nil {
		return fmt.Errorf("failed to run database auto migrations: %w", err)
	}

	storeFS := snapshotstore.NewLocalSnapshotStore(cfg.SnapshotBasePath)
	_ = storeFS

	// Stores
	repoStore := repo.NewStore(db.GormDB)
	snapshotStore := snapshot.NewStore(db.GormDB)
	indexStore := repoindex.NewStore(db.GormDB)
	diagnosisStore := diagnosis.NewStore(db.GormDB)
	reportStore := evidence.NewReportStore(db.GormDB)
	citationStore := evidence.NewCitationStore(db.GormDB)
	traceStore := trace.NewStore(db.GormDB)

	// Services
	repoSvc := repo.NewService(repoStore)
	diagnosisSvc := diagnosis.NewService(diagnosisStore, repoStore, snapshotStore)

	// Handlers
	repoHandler := repo.NewHandler(repoSvc, snapshotStore, indexStore, db.GormDB)
	diagnosisHandler := diagnosis.NewHandler(diagnosisSvc, reportStore, citationStore, traceStore)

	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(gin.Recovery())

	// Logging & RequestID Middleware with Metrics
	router.Use(func(c *gin.Context) {
		reqID := c.GetHeader("X-Request-ID")
		if reqID == "" {
			reqID = time.Now().Format("20060102150405.000000")
		}
		c.Set(string(logger.RequestIDKey), reqID)
		c.Header("X-Request-ID", reqID)
		// Default local user ID for single-tenant local runtime
		c.Set(string(logger.UserIDKey), "local-user")
		c.Next()

		status := strconv.Itoa(c.Writer.Status())
		path := c.FullPath()
		if path == "" {
			path = "unknown"
		}
		metrics.HttpRequestsTotal.WithLabelValues(c.Request.Method, path, status).Inc()
	})

	// Health & Metrics
	router.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "service": "repolens-api"})
	})
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// REST API (v1 / v2 compatible routes)
	v1 := router.Group("/api/v1")
	{
		// Repositories & Indexing
		v1.POST("/repositories", repoHandler.Register)
		v1.GET("/repositories", repoHandler.List)
		v1.GET("/repositories/:id", repoHandler.Get)
		v1.POST("/repositories/:id/index", repoHandler.TriggerIndex)

		// Diagnoses
		v1.POST("/diagnoses", diagnosisHandler.Create)
		v1.GET("/diagnoses", diagnosisHandler.List)
		v1.GET("/diagnoses/:id", diagnosisHandler.Get)
		v1.POST("/diagnoses/:id/cancel", diagnosisHandler.Cancel)
		v1.GET("/diagnoses/:id/attempts", diagnosisHandler.ListAttempts)
		v1.GET("/diagnoses/:id/report", diagnosisHandler.GetReport)
		v1.GET("/diagnoses/:id/steps", diagnosisHandler.GetSteps)
	}

	// Legacy root routes alias for backward compatibility
	root := router.Group("")
	{
		root.POST("/repositories", repoHandler.Register)
		root.GET("/repositories", repoHandler.List)
		root.GET("/repositories/:id", repoHandler.Get)
		root.POST("/repositories/:id/index", repoHandler.TriggerIndex)

		root.POST("/diagnoses", diagnosisHandler.Create)
		root.GET("/diagnoses", diagnosisHandler.List)
		root.GET("/diagnoses/:id", diagnosisHandler.Get)
		root.POST("/diagnoses/:id/cancel", diagnosisHandler.Cancel)
		root.GET("/diagnoses/:id/attempts", diagnosisHandler.ListAttempts)
		root.GET("/diagnoses/:id/report", diagnosisHandler.GetReport)
		root.GET("/diagnoses/:id/steps", diagnosisHandler.GetSteps)
	}

	srv := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: router,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server failed", "error", err)
			errCh <- err
		}
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-sigChan:
		log.Info("received shutdown signal", "signal", sig.String())
	case err := <-errCh:
		log.Error("fatal http server error, triggering immediate shutdown", "error", err)
		return fmt.Errorf("http server failed to listen: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Error("shutdown error", "error", err)
	}

	return nil
}
