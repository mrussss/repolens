package shutdown

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"repolens/internal/platform/logger"
)

type Coordinator struct {
	mu        sync.Mutex
	callbacks []func(ctx context.Context) error
}

func NewCoordinator() *Coordinator {
	return &Coordinator{
		callbacks: make([]func(ctx context.Context) error, 0),
	}
}

func (c *Coordinator) Register(cb func(ctx context.Context) error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.callbacks = append(c.callbacks, cb)
}

func (c *Coordinator) WaitForSignal(timeout time.Duration) {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	sig := <-sigChan
	logger.L(context.Background()).Info("received shutdown signal", "signal", sig.String())

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c.mu.Lock()
	defer c.mu.Unlock()

	for i := len(c.callbacks) - 1; i >= 0; i-- {
		if err := c.callbacks[i](ctx); err != nil {
			logger.L(ctx).Error("shutdown callback error", "error", err)
		}
	}
	logger.L(context.Background()).Info("graceful shutdown completed")
}
