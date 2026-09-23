package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"repolens/internal/diagnosis"
	"repolens/internal/jobs"
	"repolens/internal/llm"
	platformconfig "repolens/internal/platform/config"
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
	ErrInvalidProviderConfig         = errors.New("invalid provider configuration")
	ErrProviderConfigSaveFailed      = errors.New("failed to save provider configuration")
	ErrProviderConfigClearFailed     = errors.New("failed to clear provider configuration")
	ErrProviderCapabilityUnsupported = errors.New("provider capability unsupported")
	ErrProviderProbeTruncated        = errors.New("provider compatibility probe was truncated")
	ErrProviderResponseFormatInvalid = errors.New("provider compatibility response format invalid")
)

const (
	// providerConnectionTestTimeout is the overall budget shared by the basic
	// connectivity request and the subsequent compatibility probe. Production
	// Agent calls have their separate frozen per-request timeout.
	providerConnectionTestTimeout    = 60 * time.Second
	providerConnectionTestMaxTokens  = 256
	providerConnectionBasicMaxTokens = 32
)

const (
	ProviderTestCodeAuthFailed            = "PROVIDER_AUTH_FAILED"
	ProviderTestCodeRateLimited           = "PROVIDER_RATE_LIMITED"
	ProviderTestCodeTimeout               = "PROVIDER_TIMEOUT"
	ProviderTestCodeModelNotFound         = "PROVIDER_MODEL_NOT_FOUND"
	ProviderTestCodeUpstreamError         = "PROVIDER_UPSTREAM_ERROR"
	ProviderTestCodeConnectionError       = "PROVIDER_CONNECTION_FAILED"
	ProviderTestCodeCapabilityUnsupported = "PROVIDER_CAPABILITY_UNSUPPORTED"
	ProviderTestCodeProbeTruncated        = "PROVIDER_PROBE_TRUNCATED"
	ProviderTestCodeResponseFormatInvalid = "PROVIDER_RESPONSE_FORMAT_INVALID"
)

// CompatibilityProbeResult describes only the non-sensitive request shape
// exercised by the provider probe. It deliberately excludes credentials and
// response content.
type CompatibilityProbeResult struct {
	ProbeMaxOutputTokens      int    `json:"probe_max_output_tokens"`
	ProductionMaxOutputTokens int    `json:"production_max_output_tokens"`
	ReasoningEffort           string `json:"reasoning_effort"`
	ResponseFormat            string `json:"response_format"`
	Tools                     bool   `json:"tools"`
	ProbeStatus               string `json:"probe_status"`
	ToolCallObserved          bool   `json:"tool_call_observed"`
}

const (
	CompatibilityProbeConfirmed = "CONFIRMED"
	CompatibilityProbeUncertain = "UNCERTAIN"
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
	latency, _, err := m.TestConnectionCompatibilityWithAuthMode(ctx, baseURL, model, apiKey, authMode)
	return latency, err
}

// TestConnectionCompatibilityWithAuthMode probes the same optional generation
// capabilities used by the production Agent request. The probe uses a small
// output budget, but still requires the provider to accept reasoning_effort,
// response_format, and tools. A returned probe tool call confirms tool
// behavior; CONFIRMED means only that the first target tool call was observed,
// not that a complete multi-turn tool loop was exercised.
func (m *Manager) TestConnectionCompatibilityWithAuthMode(ctx context.Context, baseURL, model, apiKey, authMode string) (time.Duration, CompatibilityProbeResult, error) {
	normBase, err := NormalizeBaseURL(baseURL)
	if err != nil {
		return 0, CompatibilityProbeResult{}, err
	}
	cfg := platformconfig.Load()
	probe := CompatibilityProbeResult{
		ProbeMaxOutputTokens:      providerConnectionTestMaxTokens,
		ProductionMaxOutputTokens: cfg.MaxOutputTokens,
		ReasoningEffort:           cfg.ReasoningEffort,
		ResponseFormat:            "json_object",
		Tools:                     true,
		ProbeStatus:               CompatibilityProbeUncertain,
	}

	start := time.Now()
	provider := llm.NewOpenAICompatibleProviderWithAuthModeAndTimeout(apiKey, normBase, model, authMode, providerConnectionTestTimeout)

	testCtx, cancel := context.WithTimeout(ctx, providerConnectionTestTimeout)
	defer cancel()

	// First establish ordinary connectivity/authentication/model validity without
	// optional generation capabilities. This prevents an ordinary provider 400
	// from being misreported as a capability failure.
	temperature := 0.0
	if _, err := provider.Generate(testCtx, llm.GenerateRequest{
		Messages:    []llm.Message{{Role: llm.RoleUser, Content: "Reply with OK."}},
		Temperature: &temperature,
		MaxTokens:   providerConnectionBasicMaxTokens,
	}); err != nil {
		return time.Since(start), probe, fmt.Errorf("connection test failed: %w", err)
	}

	// The second, bounded probe validates the optional generation shape used by
	// the production Agent. It intentionally does not send tool_choice because
	// production Agent requests do not send it either. The budget is deliberately
	// larger than the old 32 token probe so a normal tool response is not mistaken
	// for unsupported capabilities merely because it was truncated.
	response, err := provider.Generate(testCtx, llm.GenerateRequest{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: "You are a compatibility probe. The response format is JSON. Call the provided function exactly once."},
			{Role: llm.RoleUser, Content: "Call repolens_compatibility_probe with an empty JSON object."},
		},
		Tools: []llm.ToolDefinition{{
			Type: "function",
			Function: llm.ToolFunction{
				Name:        "repolens_compatibility_probe",
				Description: "Confirms that the provider accepts RepoLens tool calls.",
				Parameters:  map[string]interface{}{"type": "object", "additionalProperties": false},
			},
		}},
		Temperature:     &temperature,
		MaxTokens:       providerConnectionTestMaxTokens,
		ReasoningEffort: cfg.ReasoningEffort,
		ResponseFormat:  &llm.ResponseFormat{Type: "json_object"},
	})
	latency := time.Since(start)

	if err != nil {
		if isExplicitCapabilityRejection(err) {
			return latency, probe, fmt.Errorf("%w: provider rejected the production Agent generation options", ErrProviderCapabilityUnsupported)
		}
		if isProbeTruncated(err) {
			return latency, probe, fmt.Errorf("%w: provider did not finish the bounded compatibility probe", ErrProviderProbeTruncated)
		}
		return latency, probe, fmt.Errorf("connection test failed: %w", err)
	}
	if strings.EqualFold(response.FinishReason, "length") {
		return latency, probe, ErrProviderProbeTruncated
	}
	if len(response.Message.ToolCalls) == 0 {
		// A provider may accept tools but choose not to call one without an
		// explicit tool_choice. It is inconclusive only if the JSON-mode response
		// itself is one complete JSON object.
		if !validJSONObject(response.Message.Content) {
			return latency, probe, ErrProviderResponseFormatInvalid
		}
		return latency, probe, nil
	}
	if len(response.Message.ToolCalls) != 1 {
		return latency, probe, fmt.Errorf("%w: provider returned an invalid compatibility tool call", ErrProviderCapabilityUnsupported)
	}
	call := response.Message.ToolCalls[0]
	if call.Type != "function" || call.Function.Name != "repolens_compatibility_probe" || !validEmptyJSONObject(call.Function.Arguments) {
		return latency, probe, fmt.Errorf("%w: provider returned an invalid compatibility tool call", ErrProviderCapabilityUnsupported)
	}
	probe.ToolCallObserved = true
	probe.ProbeStatus = CompatibilityProbeConfirmed

	return latency, probe, nil
}

func isExplicitCapabilityRejection(err error) bool {
	var httpErr *llm.HTTPError
	if !errors.As(err, &httpErr) || (httpErr.StatusCode != http.StatusBadRequest && httpErr.StatusCode != http.StatusUnprocessableEntity) {
		return false
	}
	body := strings.ToLower(httpErr.Body)
	for _, marker := range []string{"reasoning_effort", "response_format", "tools"} {
		if strings.Contains(body, marker) {
			return true
		}
	}
	return (strings.Contains(body, "unsupported") || strings.Contains(body, "unknown")) && strings.Contains(body, "parameter")
}

func isProbeTruncated(err error) bool {
	var httpErr *llm.HTTPError
	if errors.As(err, &httpErr) {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "finish_reason") && strings.Contains(strings.ToLower(err.Error()), "length")
}

func validEmptyJSONObject(raw string) bool {
	if strings.TrimSpace(raw) == "" || !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return false
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil || object == nil || len(object) != 0 {
		return false
	}
	var extra interface{}
	return decoder.Decode(&extra) == io.EOF
}

func validJSONObject(raw string) bool {
	decoder := json.NewDecoder(strings.NewReader(raw))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil || object == nil {
		return false
	}
	var extra interface{}
	return decoder.Decode(&extra) == io.EOF
}

// ClassifyTestConnectionError maps provider failures to stable, safe API
// categories. The underlying error remains available for server logs only.
func ClassifyTestConnectionError(err error) (code, message string, status int) {
	if err == nil {
		return "", "", http.StatusOK
	}
	if errors.Is(err, ErrProviderCapabilityUnsupported) {
		return ProviderTestCodeCapabilityUnsupported, "Provider 不支持 RepoLens production Agent 所需的 generation options", http.StatusBadGateway
	}
	if errors.Is(err, ErrProviderProbeTruncated) {
		return ProviderTestCodeProbeTruncated, "Provider compatibility probe 未在有限输出预算内完成", http.StatusBadGateway
	}
	if errors.Is(err, ErrProviderResponseFormatInvalid) {
		return ProviderTestCodeResponseFormatInvalid, "Provider 未按要求返回单一完整 JSON 对象", http.StatusBadGateway
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
			return ProviderTestCodeRateLimited, classifyRateLimitMessage(httpErr.Body), http.StatusTooManyRequests
		case http.StatusNotFound:
			return ProviderTestCodeModelNotFound, "Provider 模型或接口不存在，请检查 Base URL 和模型名称", http.StatusNotFound
		case http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return ProviderTestCodeUpstreamError, "Provider 上游服务暂时不可用", http.StatusBadGateway
		}
	}
	return ProviderTestCodeConnectionError, "无法连接 Provider，请检查网络和 Base URL", http.StatusBadGateway
}

type upstreamErrorPayload struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Error   *struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"error"`
}

func classifyRateLimitMessage(body string) string {
	const genericMessage = "请求过于频繁，请稍后重试（429）"

	var payload upstreamErrorPayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return genericMessage
	}

	code := normalizeUpstreamCode(payload.Code)
	if payload.Error != nil {
		if nestedCode := normalizeUpstreamCode(payload.Error.Code); nestedCode != "" {
			code = nestedCode
		}
	}

	// AIHubMix uses code 1310 for a model's exhausted monthly quota. Keep this
	// provider-specific detail in the user-facing message while hiding the raw
	// upstream response and any request identifiers.
	if code == "1310" {
		return "该模型当前月度额度已耗尽（AIHubMix code 1310）"
	}
	return genericMessage
}

func normalizeUpstreamCode(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var numericCode int
	if err := json.Unmarshal(raw, &numericCode); err == nil {
		return strconv.Itoa(numericCode)
	}

	var stringCode string
	if err := json.Unmarshal(raw, &stringCode); err == nil {
		return strings.TrimSpace(stringCode)
	}

	return ""
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
