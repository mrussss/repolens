package llm

import (
	"context"
	"errors"
	"testing"
)

type retryProviderSpy struct {
	calls int
}

func (s *retryProviderSpy) Generate(context.Context, GenerateRequest) (GenerateResponse, error) {
	s.calls++
	if s.calls == 1 {
		return GenerateResponse{}, &HTTPError{StatusCode: 429, Body: "rate limited"}
	}
	return GenerateResponse{Message: Message{Role: RoleAssistant, Content: "ok"}}, nil
}

func TestRetryingProviderRetriesOnlyDeclaredTemporaryFailures(t *testing.T) {
	spy := &retryProviderSpy{}
	response, err := NewRetryingProvider(spy, 1).Generate(context.Background(), GenerateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if spy.calls != 2 || response.ProviderAttempts != 2 {
		t.Fatalf("provider calls/attempts = %d/%d, want 2/2", spy.calls, response.ProviderAttempts)
	}

	noRetry := &errorProvider{err: context.DeadlineExceeded}
	if _, err := NewRetryingProvider(noRetry, 3).Generate(context.Background(), GenerateRequest{}); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout was unexpectedly retried or wrapped incorrectly: %v", err)
	}
	if noRetry.calls != 1 {
		t.Fatalf("timeout calls = %d, want 1", noRetry.calls)
	}
}

func TestRetryingProviderRetriesServerErrorsButNotBadRequests(t *testing.T) {
	for _, tt := range []struct {
		name      string
		status    int
		wantCalls int
		wantError bool
	}{
		{name: "server error", status: 503, wantCalls: 2},
		{name: "bad request", status: 400, wantCalls: 1, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			provider := &statusRetryProvider{status: tt.status}
			_, err := NewRetryingProvider(provider, 1).Generate(context.Background(), GenerateRequest{})
			if (err != nil) != tt.wantError || provider.calls != tt.wantCalls {
				t.Fatalf("error=%v calls=%d, want error=%t calls=%d", err, provider.calls, tt.wantError, tt.wantCalls)
			}
		})
	}
}

type statusRetryProvider struct {
	status int
	calls  int
}

func (p *statusRetryProvider) Generate(context.Context, GenerateRequest) (GenerateResponse, error) {
	p.calls++
	if p.calls == 1 {
		return GenerateResponse{}, &HTTPError{StatusCode: p.status, Body: "injected provider response"}
	}
	return GenerateResponse{Message: Message{Role: RoleAssistant, Content: "ok"}}, nil
}

type errorProvider struct {
	err   error
	calls int
}

func (p *errorProvider) Generate(context.Context, GenerateRequest) (GenerateResponse, error) {
	p.calls++
	return GenerateResponse{}, p.err
}
