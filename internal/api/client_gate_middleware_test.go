package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestClientGateMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{}
	cfg.Routing.AllowedClients = []string{"claude-code", "codex"}
	s := &Server{cfg: cfg}

	engine := gin.New()
	engine.Use(s.clientGateMiddleware())
	engine.Any("/*path", func(c *gin.Context) { c.Status(http.StatusOK) })

	cases := []struct {
		method, path, ua string
		want             int
	}{
		{http.MethodPost, "/v1/messages", "claude-cli/2.1.310 (external, cli)", http.StatusOK},
		{http.MethodGet, "/v1/responses", "codex_cli_rs/0.164.0 (Mac OS 27.2.0; arm64)", http.StatusOK},
		{http.MethodPost, "/v1/chat/completions", "OpenAI/Python 1.0", http.StatusForbidden},
		{http.MethodPost, "/v1/messages", "curl/8.7.1", http.StatusForbidden},
		{http.MethodPost, "/v1beta/models/x:generateContent", "", http.StatusForbidden},
		{http.MethodGet, "/v0/management/auth-files", "curl/8.7.1", http.StatusOK},
		{http.MethodHead, "/api/hello", "", http.StatusOK},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("User-Agent", tc.ua)
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s ua=%q: status %d, want %d", tc.method, tc.path, tc.ua, rec.Code, tc.want)
		}
	}

	cfg.Routing.AllowedClients = nil
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("User-Agent", "OpenAI/Python 1.0")
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("empty allowed-clients must allow every client, got %d", rec.Code)
	}
}
