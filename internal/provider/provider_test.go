package provider_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	"repolens/internal/provider"
)

func TestNormalizeBaseURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
		hasErr   bool
	}{
		{"https://api.openai.com/v1", "https://api.openai.com/v1", false},
		{"https://api.openai.com/v1/", "https://api.openai.com/v1", false},
		{"  HTTPS://API.OPENAI.COM/v1  ", "https://api.openai.com/v1", false},
		{"http://localhost:80/v1", "http://localhost/v1", false},
		{"https://localhost:443/v1", "https://localhost/v1", false},
		{"https://api.deepseek.com", "https://api.deepseek.com", false},
		{"", "", true},
		{"ftp://invalid.com", "", true},
		{"https://api.openai.com/v1?token=123", "", true}, // query params rejected
		{"https://api.openai.com/v1#section", "", true},   // fragments rejected
	}

	for _, tt := range tests {
		got, err := provider.NormalizeBaseURL(tt.input)
		if (err != nil) != tt.hasErr {
			t.Errorf("NormalizeBaseURL(%q) err = %v, wantErr = %v", tt.input, err, tt.hasErr)
		}
		if err == nil && got != tt.expected {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestFingerprintCalculations(t *testing.T) {
	fp1 := provider.ComputeEndpointFingerprint("https://api.openai.com/v1")
	fp2 := provider.ComputeEndpointFingerprint("https://api.openai.com/v1")
	fp3 := provider.ComputeEndpointFingerprint("https://api.deepseek.com/v1")

	if fp1 != fp2 {
		t.Errorf("fingerprint should be deterministic: %s != %s", fp1, fp2)
	}
	if fp1 == fp3 {
		t.Errorf("different endpoints must have different fingerprints: %s == %s", fp1, fp3)
	}

	cfgFp1 := provider.ComputeConfigFingerprint("https://api.openai.com/v1", "gpt-4o")
	cfgFp2 := provider.ComputeConfigFingerprint("https://api.openai.com/v1", "gpt-4o-mini")
	if cfgFp1 == cfgFp2 {
		t.Errorf("different models must have different config fingerprints")
	}
}

func TestAtomicSecretPersistence(t *testing.T) {
	tempDir := t.TempDir()
	secretFile := filepath.Join(tempDir, "provider.json")

	mgr := provider.NewManager(secretFile, "", "", "", "")

	// 1. Initial status should be unconfigured
	status := mgr.GetPublicStatus()
	if status.IsConfigured {
		t.Errorf("expected not configured initially")
	}

	// 2. Save config
	err := mgr.SaveConfig("https://api.openai.com/v1/", "gpt-4o", "sk-secret123456", false)
	if err != nil {
		t.Fatalf("SaveConfig failed: %v", err)
	}

	// 3. Verify file permissions 0600
	info, err := os.Stat(secretFile)
	if err != nil {
		t.Fatalf("stat secret file failed: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0600 {
		t.Errorf("expected 0600 permissions, got %o", perm)
	}

	// 4. Verify public status does not leak secret
	statusAfter := mgr.GetPublicStatus()
	if !statusAfter.IsConfigured {
		t.Errorf("expected is_configured=true after save")
	}
	if statusAfter.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("expected normalized base URL, got %s", statusAfter.BaseURL)
	}
	if statusAfter.Model != "gpt-4o" {
		t.Errorf("expected model gpt-4o, got %s", statusAfter.Model)
	}

	// 5. Verify secret retrieval
	sec, err := mgr.GetSecretConfig()
	if err != nil || sec.APIKey != "sk-secret123456" {
		t.Errorf("failed retrieving secret config: %v", err)
	}
}

func TestTestConnection(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-test",
			"choices": [{"message": {"role": "assistant", "content": "pong"}}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1}
		}`))
	}))
	defer mockServer.Close()

	mgr := provider.NewManager("", "", "", "", "")
	latency, err := mgr.TestConnection(context.Background(), mockServer.URL, "gpt-4o", "test-key")
	if err != nil {
		t.Fatalf("TestConnection failed: %v", err)
	}
	if latency <= 0 {
		t.Errorf("expected positive latency")
	}
}

func TestOpenAICompatibleAuthModesAndEndpointNormalization(t *testing.T) {
	var gotPath, gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	withToken := llm.NewOpenAICompatibleProviderWithAuthMode("arbitrary-token", server.URL+"/v1/", "model", "bearer")
	if _, err := withToken.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "ping"}}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "Bearer arbitrary-token" {
		t.Fatalf("unexpected bearer request path=%q auth=%q", gotPath, gotAuth)
	}

	withoutAuth := llm.NewOpenAICompatibleProviderWithAuthMode("ignored", server.URL+"/v1/", "model", "none")
	if _, err := withoutAuth.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "ping"}}}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" || gotAuth != "" {
		t.Fatalf("unexpected no-auth request path=%q auth=%q", gotPath, gotAuth)
	}
	if provider.ComputeConfigFingerprint(server.URL, "model", "none") == provider.ComputeConfigFingerprint(server.URL, "model", "bearer") {
		t.Fatal("auth mode must be part of provider fingerprint")
	}
}

func TestBuildForDiagnosisPinsMetadataButReloadsRotatedKey(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()

	mgr := provider.NewManager(filepath.Join(t.TempDir(), "provider.json"), "", "", "", "")
	if err := mgr.SaveConfig(server.URL, "model-a", "key-a", false); err != nil {
		t.Fatal(err)
	}
	normalized, err := provider.NormalizeBaseURL(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	run := &diagnosis.DiagnosisRun{
		ProviderEndpointFingerprint: provider.ComputeEndpointFingerprint(normalized),
		ProviderConfigFingerprint:   provider.ComputeConfigFingerprint(normalized, "model-a"),
		ModelName:                   "model-a",
	}

	if err := mgr.SaveConfig(server.URL, "model-a", "key-b", false); err != nil {
		t.Fatal(err)
	}
	p, err := mgr.BuildForDiagnosis(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Generate(context.Background(), llm.GenerateRequest{Messages: []llm.Message{{Role: llm.RoleUser, Content: "ping"}}}); err != nil {
		t.Fatal(err)
	}
	if authorization != "Bearer key-b" {
		t.Fatalf("worker did not reload rotated key: %q", authorization)
	}

	if err := mgr.SaveConfig(server.URL, "model-b", "key-c", false); err != nil {
		t.Fatal(err)
	}
	_, err = mgr.BuildForDiagnosis(context.Background(), run)
	if err == nil {
		t.Fatal("expected provider model drift to be rejected")
	}
	class, code := jobs.ClassifyError(err)
	if class != jobs.ErrorClassPermanent || code != "PROVIDER_CONFIG_MISMATCH" {
		t.Fatalf("expected permanent provider mismatch, got class=%s code=%s err=%v", class, code, err)
	}
}
