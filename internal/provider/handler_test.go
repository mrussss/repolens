package provider_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

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

func performProviderRequest(router http.Handler, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodPost, "/settings/provider", bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}
