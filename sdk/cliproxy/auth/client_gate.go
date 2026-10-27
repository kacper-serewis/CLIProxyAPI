package auth

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/gin-gonic/gin"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	// ClientKindClaudeCode identifies the Claude Code CLI, including the Agent SDK it embeds.
	ClientKindClaudeCode = "claude-code"
	// ClientKindCodex identifies the Codex CLI, desktop app, and exec mode.
	ClientKindCodex = "codex"
)

// claudeCodeClientUserAgentPattern matches the native Claude Code CLI user agent, for example
// "claude-cli/2.1.310 (external, cli)" or "claude-cli/2.1.310 (external, sdk-ts, agent-sdk/0.3.1)".
var claudeCodeClientUserAgentPattern = regexp.MustCompile(`(?i)^claude-cli/[0-9]+\.[0-9]+\.[0-9]+\s+\(external,`)

var claudeOAuthRequiresClaudeCode atomic.Bool

// SetClaudeOAuthRequiresClaudeCode restricts Claude OAuth (subscription) credentials to
// requests sent by the Claude Code CLI. Other clients never see those credentials as
// selection candidates, so a rejected request leaves credential state untouched.
func SetClaudeOAuthRequiresClaudeCode(enabled bool) {
	claudeOAuthRequiresClaudeCode.Store(enabled)
}

// ClientKindFromUserAgent classifies a request's user agent as ClientKindClaudeCode,
// ClientKindCodex, or "" when it is neither.
func ClientKindFromUserAgent(userAgent string) string {
	userAgent = strings.TrimSpace(userAgent)
	switch {
	case claudeCodeClientUserAgentPattern.MatchString(userAgent):
		return ClientKindClaudeCode
	case IsCodexClientUserAgent(userAgent):
		return ClientKindCodex
	default:
		return ""
	}
}

// requestUserAgent prefers the inbound client request, falling back to the executor headers.
func requestUserAgent(ctx context.Context, opts cliproxyexecutor.Options) string {
	if ctx != nil {
		if ginCtx, ok := ctx.Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
			if ua := headerValueFromHTTPHeader(ginCtx.Request.Header, "User-Agent"); ua != "" {
				return ua
			}
		}
	}
	return headerValueFromHTTPHeader(http.Header(opts.Headers), "User-Agent")
}

func isClaudeOAuthAuth(auth *Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "claude") && auth.AuthKind() != AuthKindAPIKey
}
