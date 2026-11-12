package auth

import (
	"net/http"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func resetRoutingActivity() {
	routingActivity.Lock()
	routingActivity.sessions = make(map[string]*RoutingSession)
	routingActivity.Unlock()
}

func claudeCodeOpts(session string, body string) cliproxyexecutor.Options {
	headers := http.Header{}
	headers.Set("X-Claude-Code-Session-Id", session)
	return cliproxyexecutor.Options{Headers: headers, OriginalRequest: []byte(body), Metadata: map[string]any{}}
}

func TestRoutingActivityTracksBindingsAndSwitches(t *testing.T) {
	resetRoutingActivity()
	now := time.Unix(1_800_000_000, 0)
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"Fix the   login bug"}]}]}`
	a := &Auth{ID: "claude-a.json", Provider: "claude"}
	b := &Auth{ID: "claude-b.json", Provider: "claude"}

	recordRoutingActivity(a, "claude-opus-5-5", claudeCodeOpts("11111111-aaaa", body), now)
	recordRoutingActivity(a, "claude-haiku-5-5", claudeCodeOpts("11111111-aaaa", body), now.Add(time.Minute))
	recordRoutingActivity(b, "claude-opus-5-5", claudeCodeOpts("11111111-aaaa", body), now.Add(2*time.Minute))
	recordRoutingActivity(a, "claude-opus-5-5", cliproxyexecutor.Options{}, now) // no session: ignored

	sessions := (&Manager{}).RoutingSessions()
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	s := sessions[0]
	if s.Title != "Fix the login bug" || s.Auth != "claude-b.json" || s.Requests != 3 || s.Models["claude-opus-5-5"] != 2 {
		t.Fatalf("session = %+v", s)
	}
	if len(s.Switches) != 1 || s.Switches[0].From != "claude-a.json" || s.Switches[0].To != "claude-b.json" {
		t.Fatalf("switches = %+v", s.Switches)
	}
}

func TestRoutingSessionTitleReadsResponsesInput(t *testing.T) {
	body := `{"input":[{"role":"developer","content":"rules"},{"role":"user","content":[{"type":"input_text","text":"Write tests"}]}]}`
	if got := routingSessionTitle([]byte(body)); got != "Write tests" {
		t.Fatalf("title = %q", got)
	}
}

func TestRoutingSessionTitleSkipsContextHandoff(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"Context handoff (full_thread_summary):\nlong summary\n\nUser message:\nwhich account is active?"}]}`
	if got := routingSessionTitle([]byte(body)); got != "which account is active?" {
		t.Fatalf("title = %q", got)
	}
}

func TestRoutingSessionOriginFromUserAgent(t *testing.T) {
	cases := map[string]string{
		"claude-cli/2.1.310 (external, cli)":                        "cli",
		"claude-cli/2.1.310 (external, sdk-cli)":                    "print",
		"claude-cli/2.1.310 (external, sdk-ts, agent-sdk/0.9.1)":    "sdk",
		"claude-cli/2.1.310 (external, claude-vscode)":              "ide",
		"claude-cli/2.1.322 (external, t3-code, agent-sdk/0.3.321)": "app",
		"claude-cli/2.1.322 (external, sdk-cli, agent-sdk/0.3.321)": "print",
		"codex_exec/0.130.0 (Mac OS 26.2.0; arm64)":                 "print",
		"codex_cli_rs/0.130.0 (Mac OS 26.2.0; arm64) iTerm":         "cli",
		"T3 Code/1.2 (Mac OS; arm64) unknown (T3 Code; 1.2)":        "app",
		"curl/8.7.1": "other",
		"":           "",
	}
	for ua, want := range cases {
		if got := routingSessionOrigin(ua); got != want {
			t.Errorf("origin(%q) = %q, want %q", ua, got, want)
		}
	}
}

func TestRoutingActivityFillsTitleAndOriginFromLaterRequest(t *testing.T) {
	resetRoutingActivity()
	now := time.Unix(1_800_000_000, 0)
	a := &Auth{ID: "claude-a.json", Provider: "claude"}
	warm := claudeCodeOpts("print-run", `{"messages":[{"role":"user","content":"<system-reminder>x</system-reminder>"}]}`)
	warm.Headers.Set("User-Agent", "claude-cli/2.1.310 (external, sdk-cli)")
	recordRoutingActivity(a, "claude-haiku-5-5", warm, now)
	recordRoutingActivity(a, "claude-haiku-5-5", claudeCodeOpts("print-run", `{"messages":[{"role":"user","content":"Say pong"}]}`), now)
	s := (&Manager{}).RoutingSessions()[0]
	if s.Title != "Say pong" || s.Origin != "print" {
		t.Fatalf("session = %+v", s)
	}
}
