package llm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"syscall"
	"testing"
	"time"
)

func TestConnectionRefusedRemainsTypedPreDispatchFailure(t *testing.T) {
	provider := NewOpenAICompatibleProviderWithAuthModeAndTimeout("", "http://provider.invalid", "validation-model", "none", time.Second)
	t.Cleanup(provider.httpClient.CloseIdleConnections)
	dialRefused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	provider.httpClient.Transport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, dialRefused
		},
	}

	_, callErr := provider.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: RoleUser, Content: "probe"}}})
	if callErr == nil {
		t.Fatal("provider call unexpectedly succeeded after a simulated refused dial")
	}
	var opErr *net.OpError
	if !errors.As(callErr, &opErr) || opErr.Op != "dial" || !errors.Is(callErr, syscall.ECONNREFUSED) {
		t.Fatalf("provider stack did not preserve pre-dispatch dial refusal: %T %v", callErr, callErr)
	}
	var unknown *OutcomeUnknownError
	if errors.As(callErr, &unknown) {
		t.Fatalf("typed pre-dispatch connection refusal was classified as outcome unknown: %v", callErr)
	}
}

func TestDNSResolutionFailureRemainsTypedPreDispatchFailure(t *testing.T) {
	provider := NewOpenAICompatibleProviderWithAuthModeAndTimeout("", "http://provider.invalid", "validation-model", "none", time.Second)
	t.Cleanup(provider.httpClient.CloseIdleConnections)
	dnsErr := &net.DNSError{Err: "no such host", Name: "provider.invalid", IsNotFound: true}
	provider.httpClient.Transport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: dnsErr}
		},
	}

	_, callErr := provider.Generate(context.Background(), GenerateRequest{Messages: []Message{{Role: RoleUser, Content: "probe"}}})
	if callErr == nil {
		t.Fatal("provider call unexpectedly succeeded after a simulated DNS failure")
	}
	var gotDNS *net.DNSError
	if !errors.As(callErr, &gotDNS) {
		t.Fatalf("provider stack did not preserve DNS resolution failure: %T %v", callErr, callErr)
	}
	var unknown *OutcomeUnknownError
	if errors.As(callErr, &unknown) {
		t.Fatalf("definitely pre-dispatch DNS failure was classified as outcome unknown: %v", callErr)
	}
}
