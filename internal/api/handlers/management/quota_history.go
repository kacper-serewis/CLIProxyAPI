package management

import (
	"context"
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

// GetClaudeUsage returns the last usage payload the proxy read for each active Claude
// account (including the banked reset grants), so the panel does not call Anthropic's
// rate-limited usage endpoint itself.
func (h *Handler) GetClaudeUsage(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": h.authManager.ClaudeUsageSnapshots()})
}

// RefreshClaudeUsage reads one account's usage now through the proxy's single reader
// and returns the resulting snapshot. Body: {"auth_index": "..."}.
func (h *Handler) RefreshClaudeUsage(c *gin.Context) {
	var body struct {
		AuthIndex string `json:"auth_index"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	auth := h.authByIndex(body.AuthIndex)
	if auth == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth not found"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()
	snapshot, ok := h.authManager.RefreshClaudeUsage(ctx, auth.ID)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "not an active Claude account"})
		return
	}
	c.JSON(http.StatusOK, snapshot)
}
