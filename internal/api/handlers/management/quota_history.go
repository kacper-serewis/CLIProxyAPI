package management

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetQuotaHistory returns recorded credential quota observations, optionally only those
// at or after the `since` query parameter (unix seconds).
func (h *Handler) GetQuotaHistory(c *gin.Context) {
	since, _ := strconv.ParseInt(c.Query("since"), 10, 64)
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
	points, err := coreauth.ReadQuotaHistory(coreauth.QuotaHistoryPath(resolved), since)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if points == nil {
		points = []coreauth.QuotaHistoryPoint{}
	}
	c.JSON(http.StatusOK, gin.H{"points": points})
}

// GetRoutingActivity returns which credential and model each tracked session is using,
// and the order soonest-reset would assign credentials to a new session per provider.
func (h *Handler) GetRoutingActivity(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	now := time.Now()
	c.JSON(http.StatusOK, gin.H{
		"sessions": h.authManager.RoutingSessions(),
		"next": gin.H{
			"claude": h.authManager.RoutingQueue("claude", now),
			"codex":  h.authManager.RoutingQueue("codex", now),
		},
	})
}
