package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"repolens/internal/platform/config"
)

func TestAPIHTTPListenerBindsConfiguredAddress(t *testing.T) {
	tests := []struct {
		name   string
		bind   string
		wantIP string
	}{
		{name: "direct binary default", bind: "", wantIP: "127.0.0.1"},
		{name: "explicit address", bind: "127.0.0.2", wantIP: "127.0.0.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("HTTP_BIND_ADDR", tt.bind)
			t.Setenv("HTTP_PORT", "0")
			cfg := config.Load()
			listener, err := net.Listen("tcp", apiHTTPListenAddr(cfg))
			if err != nil {
				t.Fatalf("listen on configured API address %q: %v", apiHTTPListenAddr(cfg), err)
			}
			defer listener.Close()
			host, _, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatalf("split listener address %q: %v", listener.Addr(), err)
			}
			if host != tt.wantIP {
				t.Fatalf("listener host = %q, want %q", host, tt.wantIP)
			}
		})
	}
}

func TestLocalSecurityMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(localSecurityMiddleware())
	router.POST("/state", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/read", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	tests := []struct {
		name        string
		method      string
		path        string
		host        string
		origin      string
		contentType string
		want        int
	}{
		{"loopback JSON write", http.MethodPost, "/state", "127.0.0.1:8080", "", "application/json", http.StatusNoContent},
		{"missing JSON content type", http.MethodPost, "/state", "127.0.0.1:8080", "", "", http.StatusUnsupportedMediaType},
		{"foreign host", http.MethodGet, "/read", "evil.example", "", "", http.StatusBadRequest},
		{"foreign origin", http.MethodGet, "/read", "localhost:8080", "https://evil.example", "", http.StatusForbidden},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "http://"+tt.host+tt.path, nil)
			req.Host = tt.host
			if tt.origin != "" {
				req.Header.Set("Origin", tt.origin)
			}
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			res := httptest.NewRecorder()
			router.ServeHTTP(res, req)
			if res.Code != tt.want {
				t.Fatalf("status=%d, want %d", res.Code, tt.want)
			}
		})
	}
}
