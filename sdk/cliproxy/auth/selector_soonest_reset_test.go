package auth

import (
	"context"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeQuotaAuth(id string, now time.Time, longResetIn time.Duration, longUsage, shortUsage float64) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Quota: QuotaState{
			ObservedAt: now,
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(now.Add(2*time.Hour).Unix(), 10),
				"Anthropic-Ratelimit-Unified-5h-Status":      "allowed",
				"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(shortUsage, 'f', 2, 64),
				"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(now.Add(longResetIn).Unix(), 10),
				"Anthropic-Ratelimit-Unified-7d-Status":      "allowed",
				"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(longUsage, 'f', 2, 64),
			},
		},
	}
}

func pickSoonestReset(t *testing.T, auths ...*Auth) string {
	t.Helper()
	picked, err := (&SoonestResetSelector{}).Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	return picked.ID
}

func TestSoonestResetSelectorPrefersEarliestLongWindowReset(t *testing.T) {
	now := time.Now()
	late := claudeQuotaAuth("late", now, 4*24*time.Hour, 0.10, 0.10)
	soon := claudeQuotaAuth("soon", now, 20*time.Hour, 0.60, 0.10)
	mid := claudeQuotaAuth("mid", now, 2*24*time.Hour, 0.20, 0.10)
	if got := pickSoonestReset(t, late, soon, mid); got != "soon" {
		t.Fatalf("Pick() = %q, want soon", got)
	}
}

func TestSoonestResetSelectorProbesUnobservedAuthFirst(t *testing.T) {
	now := time.Now()
	soon := claudeQuotaAuth("soon", now, 20*time.Hour, 0.10, 0.10)
	fresh := &Auth{ID: "fresh", Provider: "claude"}
	if got := pickSoonestReset(t, soon, fresh); got != "fresh" {
		t.Fatalf("Pick() = %q, want fresh", got)
	}
}

func TestSoonestResetSelectorTreatsRolledOverWindowAsUnobserved(t *testing.T) {
	now := time.Now()
	soon := claudeQuotaAuth("soon", now, 20*time.Hour, 0.10, 0.10)
	rolled := claudeQuotaAuth("rolled", now, -time.Hour, 0.99, 0.10)
	if got := pickSoonestReset(t, soon, rolled); got != "rolled" {
		t.Fatalf("Pick() = %q, want rolled", got)
	}
}

func TestSoonestResetSelectorDemotesSaturatedWindows(t *testing.T) {
	now := time.Now()
	shortFull := claudeQuotaAuth("short-full", now, 10*time.Hour, 0.30, 0.95)
	longFull := claudeQuotaAuth("long-full", now, 12*time.Hour, 0.98, 0.10)
	rejected := claudeQuotaAuth("rejected", now, 14*time.Hour, 0.30, 0.10)
	rejected.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Status"] = "rejected"
	ok := claudeQuotaAuth("ok", now, 5*24*time.Hour, 0.30, 0.10)
	if got := pickSoonestReset(t, shortFull, longFull, rejected, ok); got != "ok" {
		t.Fatalf("Pick() = %q, want ok", got)
	}
	// With every credential saturated the selector still answers instead of failing.
	if got := pickSoonestReset(t, longFull, shortFull); got != "short-full" {
		t.Fatalf("all-saturated Pick() = %q, want short-full", got)
	}
}

func TestSoonestResetSelectorBreaksTiesOnShortWindowUsage(t *testing.T) {
	now := time.Now().Truncate(time.Minute)
	busy := claudeQuotaAuth("busy", now, 24*time.Hour, 0.30, 0.70)
	idle := claudeQuotaAuth("idle", now, 24*time.Hour, 0.30, 0.20)
	if got := pickSoonestReset(t, busy, idle); got != "idle" {
		t.Fatalf("Pick() = %q, want idle", got)
	}
}

func TestSoonestResetSelectorReadsCodexLongestWindow(t *testing.T) {
	now := time.Now()
	codex := func(id string, weeklyResetIn time.Duration, weeklyUsed int) *Auth {
		return &Auth{ID: id, Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
			"X-Codex-Primary-Window-Minutes":   "300",
			"X-Codex-Primary-Reset-At":         strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10),
			"X-Codex-Primary-Used-Percent":     "10",
			"X-Codex-Secondary-Window-Minutes": "10080",
			"X-Codex-Secondary-Reset-At":       strconv.FormatInt(now.Add(weeklyResetIn).Unix(), 10),
			"X-Codex-Secondary-Used-Percent":   strconv.Itoa(weeklyUsed),
		}}}
	}
	late := codex("late", 6*24*time.Hour, 5)
	soon := codex("soon", 26*time.Hour, 40)
	picked, err := (&SoonestResetSelector{}).Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{late, soon})
	if err != nil || picked.ID != "soon" {
		t.Fatalf("Pick() = %v, %v; want soon", picked, err)
	}

	// Reset-After-Seconds relative to the observation is used when Reset-At is absent.
	relative := &Auth{ID: "relative", Provider: "codex", Quota: QuotaState{ObservedAt: now, Signals: map[string]string{
		"X-Codex-Primary-Window-Minutes":      "10080",
		"X-Codex-Primary-Reset-After-Seconds": "3600",
		"X-Codex-Primary-Used-Percent":        "1",
	}}}
	picked, err = (&SoonestResetSelector{}).Pick(context.Background(), "codex", "", cliproxyexecutor.Options{}, []*Auth{soon, relative})
	if err != nil || picked.ID != "relative" {
		t.Fatalf("relative Pick() = %v, %v; want relative", picked, err)
	}
}

// A session that is already bound must keep its credential even when another credential
// now resets sooner; otherwise every switch would rewrite the prompt cache.
func TestSoonestResetSelectorKeepsSessionAffinityBinding(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	now := time.Now()
	soon := claudeQuotaAuth("soon", now, 20*time.Hour, 0.10, 0.10)
	late := claudeQuotaAuth("late", now, 4*24*time.Hour, 0.10, 0.10)
	pick := func(session string, auths ...*Auth) string {
		t.Helper()
		opts := cliproxyexecutor.Options{
			Headers:  map[string][]string{"X-Claude-Code-Session-Id": {session}},
			Metadata: make(map[string]any),
		}
		picked, err := selector.Pick(context.Background(), "claude", "claude-sonnet-5-5", opts, auths)
		if err != nil {
			t.Fatalf("Pick(%s) error = %v", session, err)
		}
		return picked.ID
	}

	if got := pick("sess-1", soon, late); got != "soon" {
		t.Fatalf("cold bind = %q, want soon", got)
	}
	// "late" now resets sooner than "soon", but sess-1 is already bound.
	sooner := claudeQuotaAuth("late", now, time.Hour, 0.10, 0.10)
	if got := pick("sess-1", soon, sooner); got != "soon" {
		t.Fatalf("bound session moved to %q, want soon", got)
	}
	// A new session follows the new ranking.
	if got := pick("sess-2", soon, sooner); got != "late" {
		t.Fatalf("new session = %q, want late", got)
	}
}

// Established sessions and new subagents leave a credential whose short window is nearly
// full, so people sharing that account keep some headroom. Below the drain threshold the
// binding stays put to keep the prompt cache warm.
func TestSoonestResetSelectorDrainsNearlyExhaustedBinding(t *testing.T) {
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Hour,
	})
	defer selector.Stop()

	now := time.Now()
	pick := func(session, agent string, auths ...*Auth) string {
		t.Helper()
		headers := map[string][]string{"X-Claude-Code-Session-Id": {session}}
		if agent != "" {
			headers["X-Claude-Code-Agent-Id"] = []string{agent}
		}
		opts := cliproxyexecutor.Options{Headers: headers, Metadata: make(map[string]any)}
		picked, err := selector.Pick(context.Background(), "claude", "claude-sonnet-5-5", opts, auths)
		if err != nil {
			t.Fatalf("Pick(%s/%s) error = %v", session, agent, err)
		}
		return picked.ID
	}

	shared := claudeQuotaAuth("shared", now, time.Hour, 0.40, 0.50)
	other := claudeQuotaAuth("other", now, 2*24*time.Hour, 0.40, 0.10)
	if got := pick("sess-1", "", shared, other); got != "shared" {
		t.Fatalf("cold bind = %q, want shared", got)
	}

	// Past the new-session threshold but below the drain threshold: the session stays.
	warm := claudeQuotaAuth("shared", now, time.Hour, 0.40, 0.85)
	if got := pick("sess-1", "", warm, other); got != "shared" {
		t.Fatalf("session at 85%% moved to %q, want shared", got)
	}
	if got := pick("sess-2", "", warm, other); got != "other" {
		t.Fatalf("new session at 85%% = %q, want other", got)
	}

	// Past the drain threshold: subagents and the session itself move and stay moved.
	full := claudeQuotaAuth("shared", now, time.Hour, 0.40, 0.92)
	if got := pick("sess-1", "sub-1", full, other); got != "other" {
		t.Fatalf("subagent of drained parent = %q, want other", got)
	}
	if got := pick("sess-1", "", full, other); got != "other" {
		t.Fatalf("drained session = %q, want other", got)
	}
	recovered := claudeQuotaAuth("shared", now, time.Hour, 0.40, 0.10)
	if got := pick("sess-1", "", recovered, other); got != "other" {
		t.Fatalf("drained session moved back to %q, want other", got)
	}

	// With nowhere better to go the binding is kept.
	busy := claudeQuotaAuth("other", now, 2*24*time.Hour, 0.40, 0.95)
	if got := pick("sess-3", "", full, busy); got != "shared" && got != "other" {
		t.Fatalf("unexpected pick %q", got)
	}
	stay := pick("sess-3", "", full, busy)
	if got := pick("sess-3", "", full, busy); got != stay {
		t.Fatalf("binding moved between two full credentials: %q -> %q", stay, got)
	}
}
