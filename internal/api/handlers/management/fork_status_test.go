package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func getForkStatus(t *testing.T, authDir string) map[string]json.RawMessage {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/v8/management/observability/fork", nil)
	h.GetForkStatus(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

func TestForkStatusServesSnapshot(t *testing.T) {
	dir := t.TempDir()
	snapshot := `{"generated_at":"2026-10-28T17:00:00Z","repos":[]}`
	if err := os.WriteFile(filepath.Join(dir, forkStatusFile), []byte(snapshot), 0o600); err != nil {
		t.Fatal(err)
	}
	body := getForkStatus(t, dir)
	if string(body["status"]) != snapshot {
		t.Fatalf("status = %s, want %s", body["status"], snapshot)
	}
	if len(body["running"]) == 0 {
		t.Fatal("running build info missing")
	}
}

func TestForkStatusWithoutSnapshot(t *testing.T) {
	body := getForkStatus(t, t.TempDir())
	if string(body["status"]) != "null" {
		t.Fatalf("status = %s, want null", body["status"])
	}
}
