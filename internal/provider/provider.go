package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"repolens/internal/llm"
)

// ProviderConfig represents stored provider credentials.
type ProviderConfig struct {
	BaseURL   string    `json:"base_url"`
	Model     string    `json:"model"`
	APIKey    string    `json:"api_key"`
	IsDemo    bool      `json:"is_demo,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PublicProviderStatus is safe to return to the UI / browser (no API key).
type PublicProviderStatus struct {
	BaseURL             string `json:"base_url"`
	Model               string `json:"model"`
	EndpointFingerprint string `json:"endpoint_fingerprint"`
	ConfigFingerprint   string `json:"config_fingerprint"`
	IsConfigured        bool   `json:"is_configured"`
	IsDemo              bool   `json:"is_demo"`
	UpdatedAt           string `json:"updated_at,omitempty"`
}

// NormalizeBaseURL normalizes an OpenAI-compatible Base URL according to Master Spec rules:
// - trim spaces
// - lowercase scheme + host
// - remove default ports (:80, :443)
// - remove trailing slash
// - preserve explicit path such as /v1
// - reject query / fragment
func NormalizeBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("base_url cannot be empty")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("invalid URL format: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid scheme: %s (must be http or https)", u.Scheme)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("base_url must not contain query parameters or fragments")
	}

	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Host)

	// Strip default ports
	if scheme == "http" && strings.HasSuffix(host, ":80") {
		host = strings.TrimSuffix(host, ":80")
	}
	if scheme == "https" && strings.HasSuffix(host, ":443") {
		host = strings.TrimSuffix(host, ":443")
	}

	path := strings.TrimRight(u.Path, "/")
	normalized := fmt.Sprintf("%s://%s%s", scheme, host, path)
	return normalized, nil
}

// ComputeEndpointFingerprint returns SHA256(normalize(base_url)).
func ComputeEndpointFingerprint(normalizedBaseURL string) string {
	h := sha256.Sum256([]byte(normalizedBaseURL))
	return hex.EncodeToString(h[:])
}

// ComputeConfigFingerprint returns SHA256(normalize(base_url) + "|" + model).
func ComputeConfigFingerprint(normalizedBaseURL, model string) string {
	combined := fmt.Sprintf("%s|%s", normalizedBaseURL, strings.TrimSpace(model))
	h := sha256.Sum256([]byte(combined))
	return hex.EncodeToString(h[:])
}

// Manager handles reading and atomic writing of provider configuration.
type Manager struct {
	secretFilePath string
	envBaseURL     string
	envModel       string
	envAPIKey      string
	envProvider    string
	mu             sync.RWMutex
}

// NewManager creates a new Manager instance.
func NewManager(secretFilePath, envBaseURL, envModel, envAPIKey, envProvider string) *Manager {
	if secretFilePath == "" {
		secretFilePath = "/data/provider.json"
	}
	return &Manager{
		secretFilePath: secretFilePath,
		envBaseURL:     envBaseURL,
		envModel:       envModel,
		envAPIKey:      envAPIKey,
		envProvider:    envProvider,
	}
}

// GetPublicStatus returns public provider information without secrets.
func (m *Manager) GetPublicStatus() PublicProviderStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cfg, err := m.readSecretFile()
	if err == nil && cfg != nil && cfg.BaseURL != "" {
		norm, _ := NormalizeBaseURL(cfg.BaseURL)
		return PublicProviderStatus{
			BaseURL:             norm,
			Model:               cfg.Model,
			EndpointFingerprint: ComputeEndpointFingerprint(norm),
			ConfigFingerprint:   ComputeConfigFingerprint(norm, cfg.Model),
			IsConfigured:        cfg.APIKey != "" || cfg.IsDemo,
			IsDemo:              cfg.IsDemo,
			UpdatedAt:           cfg.UpdatedAt.Format(time.RFC3339),
		}
	}

	// Fallback to environment variables
	if m.envProvider == "fake" {
		return PublicProviderStatus{
			BaseURL:             "http://localhost/fake",
			Model:               "fake-gpt-4o",
			EndpointFingerprint: ComputeEndpointFingerprint("http://localhost/fake"),
			ConfigFingerprint:   ComputeConfigFingerprint("http://localhost/fake", "fake-gpt-4o"),
			IsConfigured:        true,
			IsDemo:              true,
		}
	}

	if m.envBaseURL != "" && m.envAPIKey != "" {
		norm, _ := NormalizeBaseURL(m.envBaseURL)
		return PublicProviderStatus{
			BaseURL:             norm,
			Model:               m.envModel,
			EndpointFingerprint: ComputeEndpointFingerprint(norm),
			ConfigFingerprint:   ComputeConfigFingerprint(norm, m.envModel),
			IsConfigured:        true,
			IsDemo:              false,
		}
	}

	return PublicProviderStatus{
		IsConfigured: false,
		IsDemo:       false,
	}
}

// GetSecretConfig returns the full secret config for LLM calls.
func (m *Manager) GetSecretConfig() (*ProviderConfig, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	cfg, err := m.readSecretFile()
	if err == nil && cfg != nil && cfg.BaseURL != "" {
		return cfg, nil
	}

	// Fallback to env
	if m.envProvider == "fake" {
		return &ProviderConfig{
			BaseURL: "http://localhost/fake",
			Model:   "fake-gpt-4o",
			APIKey:  "fake-key",
			IsDemo:  true,
		}, nil
	}

	if m.envBaseURL != "" && m.envAPIKey != "" {
		return &ProviderConfig{
			BaseURL: m.envBaseURL,
			Model:   m.envModel,
			APIKey:  m.envAPIKey,
			IsDemo:  false,
		}, nil
	}

	return nil, errors.New("no LLM provider configured")
}

// SaveConfig atomically writes provider credentials to disk with 0600 permissions.
func (m *Manager) SaveConfig(baseURL, model, apiKey string, isDemo bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	normalizedBase, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return fmt.Errorf("invalid base_url: %w", err)
	}

	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return errors.New("model cannot be empty")
	}

	cfg := ProviderConfig{
		BaseURL:   normalizedBase,
		Model:     trimmedModel,
		APIKey:    strings.TrimSpace(apiKey),
		IsDemo:    isDemo,
		UpdatedAt: time.Now().UTC(),
	}

	dir := filepath.Dir(m.secretFilePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed creating secrets directory: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}

	tmpFile := fmt.Sprintf("%s.tmp.%d", m.secretFilePath, time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("failed writing tmp secret file: %w", err)
	}

	// Ensure 0600 permission
	_ = os.Chmod(tmpFile, 0600)

	if err := os.Rename(tmpFile, m.secretFilePath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("failed committing secret file atomically: %w", err)
	}

	return nil
}

// TestConnection verifies that the provided BaseURL and APIKey can successfully communicate with OpenAI-compatible API.
func (m *Manager) TestConnection(ctx context.Context, baseURL, model, apiKey string) (time.Duration, error) {
	normBase, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	provider := llm.NewOpenAICompatibleProvider(apiKey, normBase, model)

	testCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// Perform a minimal dry-run completion
	_, err = provider.Generate(testCtx, llm.GenerateRequest{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "ping"},
		},
		Temperature: 0.0,
	})
	latency := time.Since(start)

	if err != nil {
		return latency, fmt.Errorf("connection test failed: %w", err)
	}

	return latency, nil
}

func (m *Manager) readSecretFile() (*ProviderConfig, error) {
	data, err := os.ReadFile(m.secretFilePath)
	if err != nil {
		return nil, err
	}
	var cfg ProviderConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
