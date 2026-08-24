package main

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"repolens/internal/auth"
	"repolens/internal/diagnosis"
	"repolens/internal/evidence"
	"repolens/internal/outbox"
	"repolens/internal/platform/config"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/metrics"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/shutdown"
	"repolens/internal/platform/snapshotstore"
	"repolens/internal/repo"
	"repolens/internal/repoindex"
	"repolens/internal/snapshot"
	"repolens/internal/sse"
	"repolens/internal/trace"
	"repolens/internal/user"
)

func main() {
	cfg := config.Load()
	logger.Init(cfg.Env)
	log := logger.L(context.Background())

	log.Info("starting RepoLens API server", "env", cfg.Env, "port", cfg.HTTPPort)

	db, err := mysql.Connect(cfg)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		return
	}
	defer db.Close()

	if err := mysql.AutoMigrate(db.GormDB); err != nil {
		log.Error("failed to run database auto migrations", "error", err)
		return
	}

	jwtManager := auth.NewJWTManager(cfg.JWTSecret, cfg.TokenTTL)
	storeFS := snapshotstore.NewLocalSnapshotStore(cfg.SnapshotBasePath)
	_ = storeFS

	// Stores
	userStore := user.NewStore(db.GormDB)
	repoStore := repo.NewStore(db.GormDB)
	snapshotStore := snapshot.NewStore(db.GormDB)
	indexStore := repoindex.NewStore(db.GormDB)
	outboxStore := outbox.NewStore(db.GormDB)
	diagnosisStore := diagnosis.NewStore(db.GormDB)
	reportStore := evidence.NewReportStore(db.GormDB)
	citationStore := evidence.NewCitationStore(db.GormDB)
	traceStore := trace.NewStore(db.GormDB)

	// Services
	userSvc := user.NewService(userStore)
	repoSvc := repo.NewService(repoStore)
	diagnosisSvc := diagnosis.NewService(diagnosisStore, repoStore, snapshotStore)

	// SSE Hub & Handler
	sseHub := sse.NewHub()
	sseHandler := sse.NewHandler(sseHub, traceStore, diagnosisStore)

	// Handlers
	userHandler := user.NewHandler(userSvc, jwtManager)
	repoHandler := repo.NewHandler(repoSvc, snapshotStore, indexStore, outboxStore, db.GormDB)
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

	// Public Auth routes
	authGroup := router.Group("/auth")
	{
		authGroup.POST("/register", userHandler.Register)
		authGroup.POST("/login", userHandler.Login)
	}

	// Protected routes
	api := router.Group("", auth.AuthMiddleware(jwtManager))
	{
		api.GET("/auth/me", userHandler.Me)

		// Repositories & Indexing
		api.POST("/repositories", repoHandler.Register)
		api.GET("/repositories", repoHandler.List)
		api.GET("/repositories/:id", repoHandler.Get)
		api.POST("/repositories/:id/index", repoHandler.TriggerIndex)

		// Diagnoses
		api.POST("/diagnoses", diagnosisHandler.Create)
		api.GET("/diagnoses", diagnosisHandler.List)
		api.GET("/diagnoses/:id", diagnosisHandler.Get)
		api.POST("/diagnoses/:id/cancel", diagnosisHandler.Cancel)
		api.GET("/diagnoses/:id/attempts", diagnosisHandler.ListAttempts)
		api.GET("/diagnoses/:id/report", diagnosisHandler.GetReport)
		api.GET("/diagnoses/:id/steps", diagnosisHandler.GetSteps)
		api.GET("/diagnoses/:id/stream", sseHandler.Stream)
	}

	srv := &http.Server{
		Addr:    ":" + cfg.HTTPPort,
		Handler: router,
	}

	coord := shutdown.NewCoordinator()
	coord.Register(func(ctx context.Context) error {
		return srv.Shutdown(ctx)
	})

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server failed", "error", err)
		}
	}()

	coord.WaitForSignal(10 * time.Second)
}
