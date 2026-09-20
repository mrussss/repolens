package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CallError preserves provider attempts when a call fails.
type CallError struct {
	Err      error
	Attempts int
}

func (e *CallError) Error() string { return e.Err.Error() }
func (e *CallError) Unwrap() error { return e.Err }

func ProviderAttempts(err error) int {
	var callErr *CallError
	if errors.As(err, &callErr) {
		return callErr.Attempts
	}
	return 0
}

type retryableProviderError interface {
	RetryableProviderError() bool
}

// RetryingProvider applies a small explicit retry budget. Only provider-
// declared temporary HTTP failures are retried; timeouts and cancellations
// are never retried here.
type RetryingProvider struct {
	delegate Provider
	maxRetry int
}

func NewRetryingProvider(delegate Provider, maxRetry int) Provider {
	if maxRetry <= 0 {
		return delegate
	}
	return &RetryingProvider{delegate: delegate, maxRetry: maxRetry}
}

func (p *RetryingProvider) Generate(ctx context.Context, req GenerateRequest) (GenerateResponse, error) {
	if p == nil || p.delegate == nil {
		return GenerateResponse{}, &CallError{Err: fmt.Errorf("provider delegate is nil")}
	}
	for attempt := 1; ; attempt++ {
		response, err := p.delegate.Generate(ctx, req)
		if err == nil {
			response.ProviderAttempts = attempt
			return response, nil
		}
		if attempt > p.maxRetry || !shouldRetryProvider(err) || ctx.Err() != nil {
			return GenerateResponse{}, &CallError{Err: err, Attempts: attempt}
		}
		timer := time.NewTimer(time.Duration(attempt) * 100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return GenerateResponse{}, &CallError{Err: ctx.Err(), Attempts: attempt}
		case <-timer.C:
		}
	}
}

func shouldRetryProvider(err error) bool {
	var declared retryableProviderError
	if errors.As(err, &declared) {
		return declared.RetryableProviderError()
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "rate limit exceeded (429)") || strings.Contains(message, "provider server error (5")
}
