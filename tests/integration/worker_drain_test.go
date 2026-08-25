package integration

import (
	"context"
	"testing"
	"time"
)

func TestWorker_TaskContextIsolationDuringDrain(t *testing.T) {
	// Simulate the consume context (parent)
	consumeCtx, cancelConsume := context.WithCancel(context.Background())
	
	// Start an in-flight task that uses WithoutCancel to isolate from the consume context
	taskCtx := context.WithoutCancel(consumeCtx)

	// Simulate that the task takes some time
	taskFinished := make(chan struct{})
	go func(ctx context.Context) {
		// Wait for 500ms simulating work
		select {
		case <-time.After(500 * time.Millisecond):
			close(taskFinished)
		case <-ctx.Done():
			// The task was wrongly canceled!
		}
	}(taskCtx)

	// Immediately trigger shutdown (cancel the consume loop)
	cancelConsume()

	// Ensure the task actually finishes naturally and is not aborted
	select {
	case <-taskFinished:
		// Success! The task survived the consume context cancellation
	case <-time.After(2 * time.Second):
		t.Fatal("task did not finish, meaning it might have hung or been improperly aborted")
	case <-taskCtx.Done():
		t.Fatal("task context was canceled when consume context was canceled - lack of isolation!")
	}
}
