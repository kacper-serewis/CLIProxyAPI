package management

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// GetChatTranscript serves one chat's conversation as the proxy last saw it.
// With ?version=N it answers {"unchanged":true} while the transcript is still at N,
// so the panel can poll cheaply.
func (h *Handler) GetChatTranscript(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	id := strings.TrimSpace(c.Query("id"))
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "id is required"})
		return
	}
	if v := c.Query("version"); v != "" {
		if known, err := strconv.ParseInt(v, 10, 64); err == nil && known >= 0 && h.authManager.ChatTranscriptVersion(id) == known {
			c.JSON(http.StatusOK, gin.H{"unchanged": true, "version": known})
			return
		}
	}
	transcript, ok := h.authManager.ChatTranscript(id)
	if !ok && transcript.Session == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "chat not found"})
		return
	}
	c.JSON(http.StatusOK, transcript)
}

// GetChatFeed serves the newest messages across all chats.
func (h *Handler) GetChatFeed(c *gin.Context) {
	if h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	var origins []string
	if raw := c.Query("origin"); raw != "" {
		origins = strings.Split(raw, ",")
	}
	c.JSON(http.StatusOK, h.authManager.ChatFeed(limit, c.Query("tools") == "1", origins...))
}
