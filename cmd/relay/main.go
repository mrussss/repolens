package main

import (
	"context"
	"time"

	"repolens/internal/mq"
	"repolens/internal/outbox"
	"repolens/internal/platform/config"
	"repolens/internal/platform/logger"
	"repolens/internal/platform/mysql"
	"repolens/internal/platform/shutdown"
)

func main() {
	cfg := config.Load()
	logger.Init(cfg.Env)
	log := logger.L(context.Background())

	log.Info("starting RepoLens Outbox Relay", "env", cfg.Env)

	db, err := mysql.Connect(cfg)
	if err != nil {
		log.Error("failed to connect to database", "error", err)
		return
	}
	defer db.Close()

	var broker mq.Broker
	rmqBroker, err := mq.NewRabbitMQBroker(cfg.RabbitMQURL)
	if err != nil {
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
}
