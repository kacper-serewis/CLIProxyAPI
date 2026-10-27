package auth

import (
	"context"
	"net/http"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestClientKindFromUserAgent(t *testing.T) {
	cases := map[string]string{
		"claude-cli/2.1.310 (external, cli)":                                                    ClientKindClaudeCode,
		"claude-cli/2.1.310 (external, sdk-ts, agent-sdk/0.3.1)":                                ClientKindClaudeCode,
		"codex_cli_rs/0.164.0 (Mac OS 27.2.0; arm64) iTerm.app":                                 ClientKindCodex,
		"codex_exec/0.164.0 (Mac OS 27.2.0; arm64)":                                             ClientKindCodex,
		"T3 Code/0.164.0 (Mac OS 27.2.0; arm64) unknown (T3 Code; 0.1.4-nightly.20261027.3770)": ClientKindCodex,
		"T3 Code/0.164.0":      "",
		"claude-cli":           "",
		"Cursor/1.0":           "",
		"python-requests/2.32": "",
		"":                     "",
	}
	for ua, want := range cases {
		if got := ClientKindFromUserAgent(ua); got != want {
			t.Errorf("ClientKindFromUserAgent(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestEligibilityHidesClaudeOAuthFromOtherClients(t *testing.T) {
	SetClaudeOAuthRequiresClaudeCode(true)
	t.Cleanup(func() { SetClaudeOAuthRequiresClaudeCode(false) })

	claudeOAuth := &Auth{ID: "claude-oauth", Provider: "claude"}
	codex := &Auth{ID: "codex", Provider: "codex"}

	eligibility := func(ua string) authSelectionEligibility {
		return authSelectionEligibilityForRequest(context.Background(), cliproxyexecutor.Options{Headers: http.Header{"User-Agent": {ua}}})
	}

	claudeCode := eligibility("claude-cli/2.1.310 (external, cli)")
	if !claudeCode.allows(claudeOAuth) || !claudeCode.allows(codex) {
		t.Fatalf("Claude Code must reach every credential")
	}
	for _, ua := range []string{"codex_cli_rs/0.164.0 (Mac OS 27.2.0; arm64)", "OpenAI/Python 1.0", ""} {
		other := eligibility(ua)
		if other.allows(claudeOAuth) {
			t.Fatalf("%q must not reach Claude OAuth credentials", ua)
		}
		if !other.allows(codex) {
			t.Fatalf("%q must still reach Codex credentials", ua)
		}
	}

	SetClaudeOAuthRequiresClaudeCode(false)
	if !eligibility("OpenAI/Python 1.0").allows(claudeOAuth) {
		t.Fatalf("with the gate off every client reaches Claude OAuth")
	}
}
