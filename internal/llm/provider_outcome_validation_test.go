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

// VALIDATION-ONLY: convert this observation to a desired-invariant regression
// test during production hardening.
func TestValidationFC07ConnectionRefusedPreservesDialBoundary(t *testing.T) {
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
	t.Logf("observation: SAFE_RETRY_DISTINGUISHABLE=YES; errors.As recovered net.OpError.Op=%q and ECONNREFUSED through HTTP/provider wrapping", opErr.Op)
}
