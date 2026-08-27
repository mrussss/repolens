package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"repolens/internal/codeintel"
	codeintelstore "repolens/internal/codeintel/store"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/indexing"
	"repolens/internal/jobs"
	"repolens/internal/platform/config"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/metrics"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/provider"
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

	if err := mysql.ApplyMigrations(db, "migrations"); err != nil {
		return fmt.Errorf("failed to run database migrations: %w", err)
	}

	storeFS := snapshotstore.NewLocalSnapshotStore(cfg.SnapshotBasePath)

	// Stores
	repoStore := repo.NewStore(db.GormDB)
	snapshotStore := snapshot.NewStore(db.GormDB)
	indexStore := repoindex.NewStore(db.GormDB)
	codeIntelStore := codeintelstore.NewStore(db.GormDB)
	diagnosisStore := diagnosis.NewStore(db.GormDB)
	reportStore := evidence.NewReportStore(db.GormDB)
	citationStore := evidence.NewCitationStore(db.GormDB)
	traceStore := trace.NewStore(db.GormDB)
	jobStore := jobs.NewStoreWithDriver(db.SqlDB, cfg.DBDriver)
	cloner := indexing.NewSafeGitCloner(cfg.AllowHosts, cfg.MaxRepoSizeMB, 2*time.Minute)

	// Services & Managers
	repoSvc := repo.NewService(repoStore)
	providerPath := cfg.ProviderSecretPath
	if providerPath == "" {
		providerPath = filepath.Join(cfg.SnapshotBasePath, "provider.json")
	}
	providerMgr := provider.NewManagerWithAuthMode(
		providerPath,
		cfg.ProviderBaseURL,
		cfg.ProviderModel,
		cfg.ProviderAPIKey,
		cfg.ProviderType,
		cfg.ProviderAuthMode,
	)
	diagnosisSvc := diagnosis.NewService(diagnosisStore, repoStore, snapshotStore).
		WithCodeIntelStore(codeIntelStore)
	diagnosisSvc.WithProviderMetadataSource(func() diagnosis.ProviderMetadata {
		status := providerMgr.GetPublicStatus()
		return diagnosis.ProviderMetadata{
			EndpointFingerprint: status.EndpointFingerprint, ConfigFingerprint: status.ConfigFingerprint,
			NormalizedBaseURL: status.BaseURL, ModelName: status.Model,
			PromptVersion: "v2.1", AgentVersion: "v2.1", AgentConfigHash: diagnosis.ComputeAgentConfigHash(8, 12, 2, 0.1), Temperature: 0.1,
		}
	})

	// Handlers
	repoHandler := repo.NewHandler(repoSvc, snapshotStore, indexStore, db.GormDB).WithSnapshotResolver(cloner, jobStore).WithSnapshotBasePath(cfg.SnapshotBasePath)
	diagnosisHandler := diagnosis.NewHandler(diagnosisSvc, reportStore, citationStore, traceStore)
	providerHandler := provider.NewHandler(
		providerMgr,
		repoStore,
		snapshotStore,
		diagnosisStore,
		reportStore,
		citationStore,
		traceStore,
		storeFS,
	).WithDemoDependencies(db.GormDB, codeIntelStore, filepath.Join(cfg.SnapshotBasePath, "indexes"))
	codeIntelHandler := codeintel.NewHandler(codeIntelStore, snapshotStore)

	if cfg.Env == "production" {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(localSecurityMiddleware())

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

	// REST API (v1)
	v1 := router.Group("/api/v1")
	{
		// System & Provider & Demo
		v1.GET("/system/provider", providerHandler.GetStatus)
		v1.POST("/system/provider", providerHandler.SaveConfig)
		v1.GET("/settings/provider", providerHandler.GetStatus)
		v1.PUT("/settings/provider", providerHandler.SaveConfig)
		v1.DELETE("/settings/provider", providerHandler.ClearConfig)
		v1.POST("/settings/provider/test", providerHandler.TestConnection)
		v1.POST("/system/provider/test", providerHandler.TestConnection)
		v1.POST("/demo/trigger", providerHandler.TriggerDemo)

		// Repositories & Indexing
		v1.POST("/repositories", repoHandler.Register)
		v1.GET("/repositories", repoHandler.List)
		v1.GET("/repositories/:id", repoHandler.Get)
		v1.POST("/repositories/:id/index", repoHandler.TriggerIndex)

		// Code Intelligence (M5)
		v1.POST("/snapshots/:id/code-index-builds", codeIntelHandler.TriggerCodeIndexBuild)
		v1.GET("/code-index-builds/:id", codeIntelHandler.GetCodeIndexBuild)
		v1.GET("/code-index-builds/:id/quality", codeIntelHandler.GetQuality)
		v1.GET("/code-index-builds/:id/symbols", codeIntelHandler.ListSymbols)
		v1.GET("/symbols/:id", codeIntelHandler.GetSymbol)
		v1.GET("/symbols/:id/references", codeIntelHandler.GetSymbolReferences)
		v1.GET("/symbols/:id/tests", codeIntelHandler.GetSymbolTests)
		v1.POST("/code-index-builds/:id/retrieval-builds", codeIntelHandler.TriggerRetrievalBuild)
		v1.GET("/retrieval-builds/:id", codeIntelHandler.GetRetrievalBuild)

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

	// Static assets & SPA fallback
	staticDir := "./web/dist"
	if _, err := os.Stat(staticDir); err == nil {
		router.Static("/assets", filepath.Join(staticDir, "assets"))
		router.StaticFile("/favicon.ico", filepath.Join(staticDir, "favicon.ico"))
		router.NoRoute(func(c *gin.Context) {
			if !strings.HasPrefix(c.Request.URL.Path, "/api/") &&
				!strings.HasPrefix(c.Request.URL.Path, "/healthz") &&
				!strings.HasPrefix(c.Request.URL.Path, "/metrics") {
				c.File(filepath.Join(staticDir, "index.html"))
				return
			}
			c.JSON(http.StatusNotFound, gin.H{"error": "route not found"})
		})
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

func localSecurityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		host := c.Request.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		allowedHost := host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "repolens-api"
		if !allowedHost {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid host"})
			return
		}
		if origin := c.GetHeader("Origin"); origin != "" {
			u, err := url.Parse(origin)
			originHost := ""
			if err == nil {
				originHost = u.Hostname()
			}
			if err != nil || (originHost != "localhost" && originHost != "127.0.0.1" && originHost != "::1") {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "origin is not allowed"})
				return
			}
		}
		if c.Request.Method == http.MethodPost || c.Request.Method == http.MethodPut || c.Request.Method == http.MethodPatch || c.Request.Method == http.MethodDelete {
			contentType := strings.ToLower(strings.TrimSpace(strings.Split(c.GetHeader("Content-Type"), ";")[0]))
			if contentType != "application/json" {
				c.AbortWithStatusJSON(http.StatusUnsupportedMediaType, gin.H{"error": "state-changing requests require application/json"})
				return
			}
		}
		c.Next()
	}
}
