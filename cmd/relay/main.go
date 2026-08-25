package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"repolens/internal/mq"
	"repolens/internal/outbox"
	"repolens/internal/platform/config"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/shutdown"
)

func main() {
	if err := run(); err != nil {
		logger.L(context.Background()).Error("outbox relay fatal error", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load()
	logger.Init(cfg.Env)
	log := logger.L(context.Background())

	log.Info("starting RepoLens Outbox Relay", "env", cfg.Env)

	db, err := mysql.Connect(cfg)
	if err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	var broker mq.Broker
	rmqBroker, err := mq.NewRabbitMQBroker(cfg.RabbitMQURL)
	if err != nil {
		if cfg.Env == "production" {
			return fmt.Errorf("failed to connect to rabbitmq in production mode: %w", err)
		}
		log.Warn("failed to connect to rabbitmq, falling back to memory broker for dev/tests", "error", err)
		broker = mq.NewMemoryBroker()
	} else {
		broker = rmqBroker
		defer broker.Close()
	}

	outboxStore := outbox.NewStore(db.GormDB)
	relay := outbox.NewRelay(outboxStore, broker, 500*time.Millisecond, 50)

	ctx, cancel := context.WithCancel(context.Background())
	coord := shutdown.NewCoordinator()
	coord.Register(func(c context.Context) error {
		cancel()
		return nil
	})

	go relay.Start(ctx)

	coord.WaitForSignal(5 * time.Second)
	return nil
}
