package management

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
)

// forkStatusFile is written next to the auth files by the local cliproxyapi-fork-status
// tool: fork commits, updater runs, local services and changed settings.
const forkStatusFile = "fork-status.json"

// GetForkStatus returns the running build and the last fork status snapshot, or a null
// status when the tool has not written one yet.
func (h *Handler) GetForkStatus(c *gin.Context) {
	cfg := h.configSnapshot()
	authDir := ""
	if cfg != nil {
		authDir = cfg.AuthDir
	}
	resolved, err := util.ResolveAuthDir(authDir)
	if err != nil || resolved == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cannot resolve auth-dir"})
		return
	}
	var status json.RawMessage
	data, err := os.ReadFile(filepath.Join(resolved, forkStatusFile))
	switch {
	case err == nil && json.Valid(data):
		status = data
	case err != nil && !errors.Is(err, os.ErrNotExist):
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"running": gin.H{
			"version":    buildinfo.Version,
			"commit":     buildinfo.Commit,
			"build_date": buildinfo.BuildDate,
		},
		"status": status,
	})
}
