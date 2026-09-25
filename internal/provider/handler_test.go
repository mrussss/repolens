package provider_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"repolens/internal/diagnosis"
	"repolens/internal/platform/mysql"
	"repolens/internal/provider"
)

func TestSaveConfigFilesystemFailureHasSafeHTTPError(t *testing.T) {
	target := filepath.Join(t.TempDir(), "not-a-directory", "provider.json")
	if err := os.WriteFile(filepath.Dir(target), []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}

	handler := provider.NewHandler(provider.NewManager(target, "", "", "", ""), nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/settings/provider", handler.SaveConfig)
	response := performProviderRequest(router, `{"base_url":"https://api.example.com/v1","model":"model","api_key":"secret-token"}`)

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "PROVIDER_CONFIG_SAVE_FAILED") || strings.Contains(body, "not-a-directory") || strings.Contains(body, "provider.json") || strings.Contains(body, "permission denied") || strings.Contains(body, "secret-token") {
		t.Fatalf("unsafe save error response: %s", body)
	}
}

func TestSaveConfigValidationHasStableHTTPError(t *testing.T) {
	handler := provider.NewHandler(provider.NewManager(filepath.Join(t.TempDir(), "provider.json"), "", "", "", ""), nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/settings/provider", handler.SaveConfig)
	response := performProviderRequest(router, `{"base_url":"not-a-url","model":"model","api_key":"secret-token"}`)

	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_PROVIDER_CONFIG") {
		t.Fatalf("validation response = %d %s", response.Code, response.Body.String())
	}
}

func TestTestConnectionFailureHasStableHTTPError(t *testing.T) {
	handler := provider.NewHandler(provider.NewManager(filepath.Join(t.TempDir(), "provider.json"), "", "", "", ""), nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/settings/provider/test", handler.TestConnection)
	response := performProviderRequestTo(router, http.MethodPost, "/settings/provider/test", `{"base_url":"http://[invalid","model":"model","api_key":"secret-token"}`)

	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "INVALID_PROVIDER_CONFIG") || strings.Contains(body, "invalid-url") || strings.Contains(body, "secret-token") {
		t.Fatalf("unsafe connection error response: %s", body)
	}
}

func TestTestConnectionQuotaFailureHasPreciseHTTPError(t *testing.T) {
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"The model's quota has reached its monthly limit.","code":1310}}`))
	}))
	defer mockServer.Close()

	handler := provider.NewHandler(provider.NewManager(filepath.Join(t.TempDir(), "provider.json"), "", "", "", ""), nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/settings/provider/test", handler.TestConnection)
	response := performProviderRequestTo(router, http.MethodPost, "/settings/provider/test", `{"base_url":"`+mockServer.URL+`","model":"model","api_key":"secret-token"}`)

	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "该模型当前月度额度已耗尽（AIHubMix code 1310）") {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestTestConnectionCapabilityFailureHasStableSafeHTTPError(t *testing.T) {
	requests := 0
	mockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if requests == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"unsupported reasoning_effort; secret-token"}}`))
	}))
	defer mockServer.Close()

	handler := provider.NewHandler(
		provider.NewManager(filepath.Join(t.TempDir(), "provider.json"), "", "", "", ""),
		nil, nil, nil, nil, nil, nil, nil,
	)
	router := gin.New()
	router.POST("/settings/provider/test", handler.TestConnection)
	response := performProviderRequestTo(router, http.MethodPost, "/settings/provider/test", `{"base_url":"`+mockServer.URL+`","model":"model","api_key":"secret-token"}`)

	if response.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, provider.ProviderTestCodeCapabilityUnsupported) || strings.Contains(body, "secret-token") || strings.Contains(body, "unsupported reasoning_effort") {
		t.Fatalf("unsafe capability response: %s", body)
	}
}

func TestClearConfigFilesystemFailureHasSafeHTTPError(t *testing.T) {
	target := filepath.Join(t.TempDir(), "secrets", "provider.json")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "sentinel"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}

	handler := provider.NewHandler(provider.NewManager(target, "", "", "", ""), nil, nil, nil, nil, nil, nil, nil)
	router := gin.New()
	router.DELETE("/settings/provider", handler.ClearConfig)
	response := performProviderRequestWithMethod(router, http.MethodDelete, "")

	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if !strings.Contains(body, "PROVIDER_CONFIG_CLEAR_FAILED") || strings.Contains(body, "secrets") || strings.Contains(body, "provider.json") || strings.Contains(body, "permission denied") {
		t.Fatalf("unsafe clear error response: %s", body)
	}
}

func TestSaveConfigRejectsActiveProviderIdentityChanges(t *testing.T) {
	tests := []struct {
		name   string
		status diagnosis.RunStatus
		base   string
		model  string
		auth   string
	}{
		{name: "endpoint", status: diagnosis.StatusQueued, base: "https://api.new.example/v1", model: "model-a", auth: "bearer"},
		{name: "model", status: diagnosis.StatusRunning, base: "https://api.old.example/v1", model: "model-b", auth: "bearer"},
		{name: "auth mode", status: diagnosis.StatusQueued, base: "https://api.old.example/v1", model: "model-a", auth: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, store, db, userID := setupProviderConfigGuard(t)
			seedProviderGuardRun(t, db, "active-"+strings.ReplaceAll(tt.name, " ", "-"), userID, tt.status, time.Now().UTC())
			before := manager.GetPublicStatus()
			response := saveProviderConfigAsUser(manager, store, userID, tt.base, tt.model, "key-next", tt.auth)
			if response.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", response.Code, response.Body.String())
			}
			after := manager.GetPublicStatus()
			if after.ConfigFingerprint != before.ConfigFingerprint {
				t.Fatalf("provider identity changed despite active diagnosis: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestSaveConfigAllowsAPIKeyRotationWithActiveDiagnosis(t *testing.T) {
	manager, store, db, userID := setupProviderConfigGuard(t)
	seedProviderGuardRun(t, db, "active-key-rotation", userID, diagnosis.StatusRunning, time.Now().UTC())
	response := saveProviderConfigAsUser(manager, store, userID, "https://api.old.example/v1", "model-a", "key-rotated", "bearer")
	if response.Code != http.StatusOK {
		t.Fatalf("key-only rotation status = %d, want 200: %s", response.Code, response.Body.String())
	}
	config, err := manager.GetSecretConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.APIKey != "key-rotated" {
		t.Fatalf("API key was not rotated: got %q", config.APIKey)
	}
}

func TestSaveConfigAllowsIdentityChangesWithoutActiveDiagnosis(t *testing.T) {
	tests := []struct {
		name  string
		base  string
		model string
		auth  string
	}{
		{name: "endpoint", base: "https://api.new.example/v1", model: "model-a", auth: "bearer"},
		{name: "model", base: "https://api.old.example/v1", model: "model-b", auth: "bearer"},
		{name: "auth mode", base: "https://api.old.example/v1", model: "model-a", auth: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, store, db, userID := setupProviderConfigGuard(t)
			seedProviderGuardRun(t, db, "terminal-"+strings.ReplaceAll(tt.name, " ", "-"), userID, diagnosis.StatusFailed, time.Now().UTC())
			response := saveProviderConfigAsUser(manager, store, userID, tt.base, tt.model, "key-next", tt.auth)
			if response.Code != http.StatusOK {
				t.Fatalf("terminal run blocked provider identity change: status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestSaveConfigFindsActiveDiagnosisBeyondRecentHundredRuns(t *testing.T) {
	manager, store, db, userID := setupProviderConfigGuard(t)
	now := time.Now().UTC()
	seedProviderGuardRun(t, db, "old-active", "another-user", diagnosis.StatusQueued, now.Add(-time.Hour))
	for i := 0; i < 100; i++ {
		seedProviderGuardRun(t, db, fmt.Sprintf("new-terminal-%03d", i), userID, diagnosis.StatusSucceeded, now.Add(time.Duration(i+1)*time.Second))
	}
	response := saveProviderConfigAsUser(manager, store, userID, "https://api.new.example/v1", "model-a", "key-next", "bearer")
	if response.Code != http.StatusConflict {
		t.Fatalf("old active diagnosis was missed: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestClearConfigRejectsAnyUsersActiveDiagnosis(t *testing.T) {
	manager, store, db, _ := setupProviderConfigGuard(t)
	seedProviderGuardRun(t, db, "other-user-active", "another-user", diagnosis.StatusQueued, time.Now().UTC())
	before := manager.GetPublicStatus()
	response := clearProviderConfigAsUser(manager, store, "provider-guard-user")
	if response.Code != http.StatusConflict {
		t.Fatalf("clear status = %d, want 409: %s", response.Code, response.Body.String())
	}
	if after := manager.GetPublicStatus(); after.ConfigFingerprint != before.ConfigFingerprint {
		t.Fatalf("provider config was cleared despite another user's active diagnosis: before=%+v after=%+v", before, after)
	}
}

func TestClearConfigFailsClosedWhenActiveRunQueryFails(t *testing.T) {
	manager, store, _, userID := setupProviderConfigGuard(t)
	before := manager.GetPublicStatus()
	response := clearProviderConfigAsUser(manager, &activeRunCheckFailureStore{Store: store}, userID)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("clear status = %d, want 500 when active-run query fails: %s", response.Code, response.Body.String())
	}
	if after := manager.GetPublicStatus(); after.ConfigFingerprint != before.ConfigFingerprint {
		t.Fatalf("provider config was cleared after active-run query failure: before=%+v after=%+v", before, after)
	}
}

func TestSaveConfigSerializesWithDiagnosisCreation(t *testing.T) {
	manager, store, db, userID := setupProviderConfigGuard(t)
	lockHeld := make(chan struct{})
	allowCreate := make(chan struct{})
	createFinished := make(chan error, 1)
	go func() {
		createFinished <- store.WithProviderConfigLock(context.Background(), func() error {
			close(lockHeld)
			<-allowCreate
			return db.Create(providerGuardRun("racing-create", userID, diagnosis.StatusQueued, time.Now().UTC())).Error
		})
	}()
	<-lockHeld

	// Once diagnosis creation owns the shared guard, a provider mutation must
	// wait until its queued row is committed and then observe it as active.
	requestFinished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		requestFinished <- saveProviderConfigAsUser(manager, store, userID, "https://api.new.example/v1", "model-a", "key-next", "bearer")
	}()
	select {
	case response := <-requestFinished:
		t.Fatalf("provider request completed before queued diagnosis creation: status=%d body=%s", response.Code, response.Body.String())
	case <-time.After(25 * time.Millisecond):
	}
	close(allowCreate)
	if err := <-createFinished; err != nil {
		t.Fatal(err)
	}
	response := <-requestFinished
	if response.Code != http.StatusConflict {
		t.Fatalf("provider identity changed after serialized diagnosis creation: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestSaveConfigFailsClosedWhenActiveRunQueryFails(t *testing.T) {
	manager, store, _, userID := setupProviderConfigGuard(t)
	before := manager.GetPublicStatus()
	failingStore := &activeRunCheckFailureStore{Store: store}
	response := saveProviderConfigAsUser(manager, failingStore, userID, "https://api.new.example/v1", "model-a", "key-next", "bearer")
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when active-run query fails: %s", response.Code, response.Body.String())
	}
	if after := manager.GetPublicStatus(); after.ConfigFingerprint != before.ConfigFingerprint {
		t.Fatalf("provider identity changed after active-run query failure: before=%+v after=%+v", before, after)
	}
}

type activeRunCheckFailureStore struct {
	diagnosis.Store
}

func (activeRunCheckFailureStore) HasActiveRuns(context.Context) (bool, error) {
	return false, errors.New("simulated active-run query failure")
}

func setupProviderConfigGuard(t *testing.T) (*provider.Manager, diagnosis.Store, *gorm.DB, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "provider_guard.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open provider guard database: %v", err)
	}
	if err := mysql.AutoMigrate(db); err != nil {
		t.Fatalf("migrate provider guard database: %v", err)
	}
	manager := provider.NewManager(filepath.Join(t.TempDir(), "provider.json"), "", "", "", "")
	if err := manager.SaveConfigWithAuthMode("https://api.old.example/v1", "model-a", "key-current", "bearer", false); err != nil {
		t.Fatalf("save initial provider config: %v", err)
	}
	return manager, diagnosis.NewStore(db), db, "provider-guard-user"
}

func seedProviderGuardRun(t *testing.T, db *gorm.DB, suffix, userID string, status diagnosis.RunStatus, createdAt time.Time) {
	t.Helper()
	if err := db.Create(providerGuardRun(suffix, userID, status, createdAt)).Error; err != nil {
		t.Fatalf("seed diagnosis %s: %v", suffix, err)
	}
}

func providerGuardRun(suffix, userID string, status diagnosis.RunStatus, createdAt time.Time) *diagnosis.DiagnosisRun {
	normalized, err := provider.NormalizeBaseURL("https://api.old.example/v1")
	if err != nil {
		panic(err)
	}
	return &diagnosis.DiagnosisRun{
		ID: "run-" + suffix, UserID: userID, RepositoryID: "repo-" + suffix, SnapshotID: "snapshot-" + suffix,
		CodeIndexBuildID: 1, RetrievalBuildID: 1, IssueTitle: "test issue", Status: status,
		ProviderEndpointFingerprint: provider.ComputeEndpointFingerprint(normalized),
		ProviderConfigFingerprint:   provider.ComputeConfigFingerprint(normalized, "model-a", "bearer"),
		NormalizedBaseURL:           normalized, ModelName: "model-a", PromptVersion: "prompt-v1", AgentVersion: "agent-v1",
		AgentConfigHash: "agent-config", IdempotencyKey: "provider-guard-" + suffix, IdempotencyRequestHash: "request-hash",
		CreatedAt: createdAt,
	}
}

func saveProviderConfigAsUser(manager *provider.Manager, store diagnosis.Store, userID, baseURL, model, apiKey, authMode string) *httptest.ResponseRecorder {
	handler := provider.NewHandler(manager, nil, nil, store, nil, nil, nil, nil)
	router := gin.New()
	router.POST("/settings/provider", func(c *gin.Context) {
		c.Set("user_id", userID)
		handler.SaveConfig(c)
	})
	body := fmt.Sprintf(`{"base_url":%q,"model":%q,"api_key":%q,"auth_mode":%q}`, baseURL, model, apiKey, authMode)
	return performProviderRequest(router, body)
}

func clearProviderConfigAsUser(manager *provider.Manager, store diagnosis.Store, userID string) *httptest.ResponseRecorder {
	handler := provider.NewHandler(manager, nil, nil, store, nil, nil, nil, nil)
	router := gin.New()
	router.DELETE("/settings/provider", func(c *gin.Context) {
		c.Set("user_id", userID)
		handler.ClearConfig(c)
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/settings/provider", nil)
	router.ServeHTTP(recorder, request)
	return recorder
}

func performProviderRequest(router http.Handler, body string) *httptest.ResponseRecorder {
	return performProviderRequestWithMethod(router, http.MethodPost, body)
}

func performProviderRequestWithMethod(router http.Handler, method, body string) *httptest.ResponseRecorder {
	return performProviderRequestTo(router, method, "/settings/provider", body)
}

func performProviderRequestTo(router http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
