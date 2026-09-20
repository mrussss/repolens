package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/llm"
)

// ProviderConfig represents stored provider credentials.
type ProviderConfig struct {
	BaseURL   string    `json:"base_url"`
	Model     string    `json:"model"`
	APIKey    string    `json:"api_key"`
	AuthMode  string    `json:"auth_mode"`
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
	AuthMode            string `json:"auth_mode"`
	UpdatedAt           string `json:"updated_at,omitempty"`
}

var (
	ErrInvalidProviderConfig     = errors.New("invalid provider configuration")
	ErrProviderConfigSaveFailed  = errors.New("failed to save provider configuration")
	ErrProviderConfigClearFailed = errors.New("failed to clear provider configuration")
)

const (
	providerConnectionTestTimeout   = 60 * time.Second
	providerConnectionTestMaxTokens = 32
)

const (
	ProviderTestCodeAuthFailed      = "PROVIDER_AUTH_FAILED"
	ProviderTestCodeRateLimited     = "PROVIDER_RATE_LIMITED"
	ProviderTestCodeTimeout         = "PROVIDER_TIMEOUT"
	ProviderTestCodeModelNotFound   = "PROVIDER_MODEL_NOT_FOUND"
	ProviderTestCodeUpstreamError   = "PROVIDER_UPSTREAM_ERROR"
	ProviderTestCodeConnectionError = "PROVIDER_CONNECTION_FAILED"
)

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

// ComputeConfigFingerprint returns SHA256(normalize(base_url) + "|" + model + "|" + auth_mode).
func ComputeConfigFingerprint(normalizedBaseURL, model string, authModes ...string) string {
	authMode := "bearer"
	if len(authModes) > 0 && authModes[0] == "none" {
		authMode = "none"
	}
	combined := fmt.Sprintf("%s|%s|%s", normalizedBaseURL, strings.TrimSpace(model), authMode)
	h := sha256.Sum256([]byte(combined))
	return hex.EncodeToString(h[:])
}

// Manager handles reading and atomic writing of provider configuration.
type Manager struct {
	secretFilePath  string
	envBaseURL      string
	envModel        string
	envAPIKey       string
	envProvider     string
	envAuthMode     string
	providerTimeout time.Duration
	providerRetries int
	mu              sync.RWMutex
}

// BuildForDiagnosis loads the current secret for every execution while using
// the identity pinned on the DiagnosisRun. This permits API-key rotation but
// prevents a queued diagnosis from silently changing endpoint or model.
func (m *Manager) BuildForDiagnosis(ctx context.Context, run *diagnosis.DiagnosisRun) (llm.Provider, error) {
	cfg, err := m.GetSecretConfig()
	if err != nil {
		return nil, err
	}
	normalized, err := NormalizeBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("invalid current provider endpoint: %w", err)
	}
	if run.ProviderEndpointFingerprint != "" && ComputeEndpointFingerprint(normalized) != run.ProviderEndpointFingerprint {
		return nil, jobs.NewPermanentError("PROVIDER_ENDPOINT_MISMATCH", "current provider endpoint differs from diagnosis pin", nil)
	}
	modelName := strings.TrimSpace(cfg.Model)
	if run.ModelName != "" && modelName != run.ModelName {
		return nil, jobs.NewPermanentError("PROVIDER_CONFIG_MISMATCH", "current provider model differs from diagnosis pin", nil)
	}
	if run.ModelName != "" {
		modelName = run.ModelName
	}
	if run.ProviderConfigFingerprint != "" && ComputeConfigFingerprint(normalized, modelName, cfg.AuthMode) != run.ProviderConfigFingerprint {
		return nil, jobs.NewPermanentError("PROVIDER_CONFIG_MISMATCH", "current provider model or authentication mode differs from diagnosis pin", nil)
	}
	if cfg.IsDemo || m.envProvider == "fake" && normalized == "http://localhost/fake" {
		return llm.NewFakeProvider(llm.ModeNormalStructured), nil
	}
	timeout := m.providerTimeout
	if run.ProviderTimeoutSeconds > 0 {
		timeout = time.Duration(run.ProviderTimeoutSeconds) * time.Second
	}
	retries := m.providerRetries
	if run.ProviderRetryAttempts >= 0 {
		retries = run.ProviderRetryAttempts
	}
	baseProvider := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout(cfg.APIKey, normalized, modelName, cfg.AuthMode, timeout)
	return llm.NewRetryingProvider(baseProvider, retries), nil
}

// NewManager creates a new Manager instance.
func NewManager(secretFilePath, envBaseURL, envModel, envAPIKey, envProvider string) *Manager {
	return NewManagerWithAuthMode(secretFilePath, envBaseURL, envModel, envAPIKey, envProvider, "bearer")
}

// NewManagerWithAuthMode creates a manager with an explicit environment auth mode.
func NewManagerWithAuthMode(secretFilePath, envBaseURL, envModel, envAPIKey, envProvider, authMode string) *Manager {
	return NewManagerWithAuthModeAndTimeout(secretFilePath, envBaseURL, envModel, envAPIKey, envProvider, authMode, 60*time.Second)
}

// NewManagerWithAuthModeAndTimeout creates a manager with an explicit
// environment auth mode and provider request timeout.
func NewManagerWithAuthModeAndTimeout(secretFilePath, envBaseURL, envModel, envAPIKey, envProvider, authMode string, providerTimeout time.Duration) *Manager {
	return NewManagerWithAuthModeAndTimeoutAndRetries(secretFilePath, envBaseURL, envModel, envAPIKey, envProvider, authMode, providerTimeout, 0)
}

// NewManagerWithAuthModeAndTimeoutAndRetries configures the bounded provider
// retry policy independently from DB-backed AnalysisJob retries.
func NewManagerWithAuthModeAndTimeoutAndRetries(secretFilePath, envBaseURL, envModel, envAPIKey, envProvider, authMode string, providerTimeout time.Duration, providerRetries int) *Manager {
	if secretFilePath == "" {
		secretFilePath = defaultProviderSecretPath()
	}
	if providerTimeout <= 0 {
		providerTimeout = 60 * time.Second
	}
	return &Manager{
		secretFilePath:  secretFilePath,
		envBaseURL:      envBaseURL,
		envModel:        envModel,
		envAPIKey:       envAPIKey,
		envProvider:     envProvider,
		envAuthMode:     normalizeAuthMode(authMode),
		providerTimeout: providerTimeout,
		providerRetries: max(0, providerRetries),
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
			AuthMode:            normalizeAuthMode(cfg.AuthMode),
			EndpointFingerprint: ComputeEndpointFingerprint(norm),
			ConfigFingerprint:   ComputeConfigFingerprint(norm, cfg.Model, cfg.AuthMode),
			IsConfigured:        cfg.APIKey != "" || cfg.IsDemo || normalizeAuthMode(cfg.AuthMode) == "none",
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
			ConfigFingerprint:   ComputeConfigFingerprint("http://localhost/fake", "fake-gpt-4o", "none"),
			AuthMode:            "none",
			IsConfigured:        true,
			IsDemo:              true,
		}
	}

	if m.envBaseURL != "" && (m.envAPIKey != "" || m.envAuthMode == "none") {
		norm, _ := NormalizeBaseURL(m.envBaseURL)
		authMode := normalizeAuthMode(m.envAuthMode)
		return PublicProviderStatus{
			BaseURL:             norm,
			Model:               m.envModel,
			EndpointFingerprint: ComputeEndpointFingerprint(norm),
			ConfigFingerprint:   ComputeConfigFingerprint(norm, m.envModel, authMode),
			AuthMode:            authMode,
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
			BaseURL:  "http://localhost/fake",
			Model:    "fake-gpt-4o",
			APIKey:   "fake-key",
			AuthMode: "none",
			IsDemo:   true,
		}, nil
	}

	if m.envBaseURL != "" && (m.envAPIKey != "" || m.envAuthMode == "none") {
		return &ProviderConfig{
			BaseURL:  m.envBaseURL,
			Model:    m.envModel,
			APIKey:   m.envAPIKey,
			AuthMode: normalizeAuthMode(m.envAuthMode),
			IsDemo:   false,
		}, nil
	}

	return nil, errors.New("no LLM provider configured")
}

// SaveConfig atomically writes provider credentials to disk with 0600 permissions.
func (m *Manager) SaveConfig(baseURL, model, apiKey string, isDemo bool) error {
	return m.SaveConfigWithAuthMode(baseURL, model, apiKey, "bearer", isDemo)
}

func (m *Manager) SaveConfigWithAuthMode(baseURL, model, apiKey, authMode string, isDemo bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	normalizedBase, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return fmt.Errorf("%w: invalid base_url: %v", ErrInvalidProviderConfig, err)
	}

	trimmedModel := strings.TrimSpace(model)
	if trimmedModel == "" {
		return fmt.Errorf("%w: model cannot be empty", ErrInvalidProviderConfig)
	}
	authMode = normalizeAuthMode(authMode)
	if authMode == "bearer" && strings.TrimSpace(apiKey) == "" && !isDemo {
		return fmt.Errorf("%w: API key cannot be empty when bearer authentication is selected", ErrInvalidProviderConfig)
	}

	cfg := ProviderConfig{
		BaseURL:   normalizedBase,
		Model:     trimmedModel,
		APIKey:    strings.TrimSpace(apiKey),
		AuthMode:  authMode,
		IsDemo:    isDemo,
		UpdatedAt: time.Now().UTC(),
	}

	dir := filepath.Dir(m.secretFilePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("%w: failed creating secrets directory: %v", ErrProviderConfigSaveFailed, err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("%w: failed setting secrets directory permissions: %v", ErrProviderConfigSaveFailed, err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("%w: marshal provider configuration: %v", ErrProviderConfigSaveFailed, err)
	}

	tmpFile := fmt.Sprintf("%s.tmp.%d", m.secretFilePath, time.Now().UnixNano())
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("%w: failed writing temporary secret file: %v", ErrProviderConfigSaveFailed, err)
	}

	// Ensure 0600 permission
	if err := os.Chmod(tmpFile, 0600); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("%w: failed setting secret file permissions: %v", ErrProviderConfigSaveFailed, err)
	}

	if err := os.Rename(tmpFile, m.secretFilePath); err != nil {
		_ = os.Remove(tmpFile)
		return fmt.Errorf("%w: failed committing secret file atomically: %v", ErrProviderConfigSaveFailed, err)
	}

	return nil
}

func defaultProviderSecretPath() string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return filepath.Join(".", ".repolens", "secrets", "provider.json")
	}
	return filepath.Join(home, ".repolens", "secrets", "provider.json")
}

// ClearConfig removes the locally persisted credential without touching env
// fallback values. The file is deliberately not read back into any response.
func (m *Manager) ClearConfig() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := os.Remove(m.secretFilePath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w: %v", ErrProviderConfigClearFailed, err)
	}
	return nil
}

// TestConnection verifies that the provided BaseURL and APIKey can successfully communicate with OpenAI-compatible API.
func (m *Manager) TestConnection(ctx context.Context, baseURL, model, apiKey string) (time.Duration, error) {
	return m.TestConnectionWithAuthMode(ctx, baseURL, model, apiKey, "bearer")
}

func (m *Manager) TestConnectionWithAuthMode(ctx context.Context, baseURL, model, apiKey, authMode string) (time.Duration, error) {
	normBase, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	provider := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout(apiKey, normBase, model, authMode, providerConnectionTestTimeout)

	testCtx, cancel := context.WithTimeout(ctx, providerConnectionTestTimeout)
	defer cancel()

	// Perform a bounded dry-run completion. Reasoning models may spend a large
	// budget even for "ping", so keep this probe cheap while allowing enough
	// time for cold starts and provider-side reasoning.
	temperature := 0.0
	_, err = provider.Generate(testCtx, llm.GenerateRequest{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: "Reply with exactly OK."},
		},
		Temperature: &temperature,
		MaxTokens:   providerConnectionTestMaxTokens,
	})
	latency := time.Since(start)

	if err != nil {
		return latency, fmt.Errorf("connection test failed: %w", err)
	}

	return latency, nil
}

// ClassifyTestConnectionError maps provider failures to stable, safe API
// categories. The underlying error remains available for server logs only.
func ClassifyTestConnectionError(err error) (code, message string, status int) {
	if err == nil {
		return "", "", http.StatusOK
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ProviderTestCodeTimeout, "Provider 请求超时（60 秒），上游可能仍在处理请求", http.StatusGatewayTimeout
	}
	var httpErr *llm.HTTPError
	if errors.As(err, &httpErr) {
		switch httpErr.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return ProviderTestCodeAuthFailed, "Provider 鉴权失败，请检查 API Key", http.StatusBadGateway
		case http.StatusTooManyRequests:
			return ProviderTestCodeRateLimited, "Provider 请求被限流或额度不足（429）", http.StatusTooManyRequests
		case http.StatusNotFound:
			return ProviderTestCodeModelNotFound, "Provider 模型或接口不存在，请检查 Base URL 和模型名称", http.StatusNotFound
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return ProviderTestCodeUpstreamError, "Provider 上游服务暂时不可用", http.StatusBadGateway
		}
	}
	return ProviderTestCodeConnectionError, "无法连接 Provider，请检查网络和 Base URL", http.StatusBadGateway
}

func normalizeAuthMode(mode string) string {
	if strings.EqualFold(strings.TrimSpace(mode), "none") {
		return "none"
	}
	return "bearer"
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
