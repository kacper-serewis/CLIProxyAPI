package auth

import (
	"context"
	"math"
	"strings"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestAdaptiveReserveHomePreferenceAndSurplus(t *testing.T) {
	now := time.Now()
	s := &AdaptiveReserveSelector{HomeAccount: "home"}
	pick := func(auths ...*Auth) string {
		t.Helper()
		a, err := s.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, auths)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	home := claudeQuotaAuth("home", now, 2*24*time.Hour, 0.2, 0.2)
	other := claudeQuotaAuth("other", now, 2*24*time.Hour, 0.2, 0.2)
	if pick(other, home) != "home" {
		t.Fatal("home must break equivalent capacity ties")
	}
	home = claudeQuotaAuth("home", now, 2*24*time.Hour, 0.6, 0.6)
	if pick(home, other) != "other" {
		t.Fatal("idle spare capacity must override home preference")
	}
	other.Quota.ObservedAt = now.Add(-time.Hour)
	if pick(home, other) != "home" {
		t.Fatal("stale account must not win over fresh surplus")
	}
	if s.decision(other, now).Eligible {
		t.Fatal("stale observation treated as spare")
	}
	home = claudeQuotaAuth("home", now, 2*24*time.Hour, 0.99, 0.99)
	other = claudeQuotaAuth("other", now, 2*24*time.Hour, 0.99, 0.99)
	if pick(other, home) != "home" {
		t.Fatal("under pressure, prefer home rather than colleague reserves")
	}
}

func demandPoints(now time.Time, quiet bool) []QuotaHistoryPoint {
	var points []QuotaHistoryPoint
	for i := 0; i <= 6; i++ {
		used := 0.1 + float64(i)*0.025
		if quiet {
			used = 0.1
		}
		at := now.Add(time.Duration(i-6) * 10 * time.Minute).Unix()
		points = append(points, QuotaHistoryPoint{At: at, Observed: at, Auth: "other", Provider: "claude", ShortUsed: used, LongUsed: used / 5, ShortReset: now.Add(2 * time.Hour).Unix(), LongReset: now.Add(24 * time.Hour).Unix()})
	}
	return points
}

func TestAdaptiveReserveQuietAccountsAndExpiryReleaseCapacity(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	w := quotaWindow{reset: now.Add(2 * time.Hour), usage: 0.2}
	busy := forecastWindow(w, demandPoints(now, false), now)
	quiet := forecastWindow(w, demandPoints(now, true), now)
	if quiet.Reserve >= busy.Reserve || quiet.Spare <= busy.Spare {
		t.Fatalf("quiet=%+v busy=%+v", quiet, busy)
	}
	near := forecastWindow(quotaWindow{reset: now.Add(5 * time.Minute), usage: 0.2}, demandPoints(now, false), now)
	if near.Reserve >= busy.Reserve || near.Buffer >= busy.Buffer {
		t.Fatal("reserve must taper as reset approaches")
	}
	week := forecastWindow(quotaWindow{long: true, reset: now.Add(6 * 24 * time.Hour), usage: 0.9}, nil, now)
	if week.Spare >= 0 {
		t.Fatal("weekly demand must protect the weekly window")
	}
}

func TestAdaptiveReserveProfileRejectsResetsGapsAndHeartbeat(t *testing.T) {
	now := time.Now()
	points := demandPoints(now, false)
	for i := 1; i < len(points); i++ {
		points[i].ShortReset += int64(i) * 60
	}
	profile := buildDemandProfile(points, false, now)
	if profile.recentHours != 0 {
		t.Fatal("reset changes counted as a rate")
	}
	h := &quotaDemandHistory{}
	a := demandPoints(now, true)[0]
	h.add([]QuotaHistoryPoint{a}, now)
	b := a
	b.At = now.Unix()
	h.add([]QuotaHistoryPoint{b}, now)
	if len(h.snapshot(a.Auth)) != 1 {
		t.Fatal("heartbeat is not a fresh observation")
	}
	p := demandPoints(now, false)
	p[1].Observed = p[0].Observed + 3600
	p = p[:2]
	if buildDemandProfile(p, false, now.Add(time.Hour)).recentHours != 0 {
		t.Fatal("long gaps are not evidence of quiet")
	}
}

func TestAdaptiveReserveProfileUsesWeekdayAndWeekendHours(t *testing.T) {
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.Local)
	profile := buildDemandProfile(demandPoints(now, false), false, now)
	weekday, learned := profile.typical(now.Add(-time.Hour), 0.04)
	weekend, weekendLearned := profile.typical(time.Date(2026, 10, 31, 11, 0, 0, 0, time.Local), 0.04)
	if !learned || weekday <= weekend || weekendLearned {
		t.Fatalf("weekday=%v/%v weekend=%v/%v", weekday, learned, weekend, weekendLearned)
	}
}

func TestAdaptiveReserveAffinityStaysUntilReserveThreatened(t *testing.T) {
	fallback := &AdaptiveReserveSelector{HomeAccount: "home"}
	s := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{Fallback: fallback, TTL: time.Hour})
	defer s.Stop()
	now := time.Now()
	home := claudeQuotaAuth("home", now, 24*time.Hour, 0.2, 0.2)
	other := claudeQuotaAuth("other", now, 24*time.Hour, 0.2, 0.2)
	pick := func(session, agent string) string {
		t.Helper()
		opts := claudeCodeOpts(session, "")
		if agent != "" {
			opts.Headers.Set("X-Claude-Code-Agent-Id", agent)
		}
		a, err := s.Pick(context.Background(), "claude", "claude-sonnet-5-5", opts, []*Auth{home, other})
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	if pick("chat", "") != "home" {
		t.Fatal("initial home binding")
	}
	home = claudeQuotaAuth("home", now, 24*time.Hour, 0.5, 0.5)
	if pick("new", "") != "other" || pick("chat", "") != "home" {
		t.Fatal("new routing should change without moving warm chat")
	}
	home = claudeQuotaAuth("home", now, 24*time.Hour, 0.5, 0.98)
	if pick("chat", "child") != "other" || pick("chat", "") != "other" {
		t.Fatal("protect reserve for both subagents and parent")
	}
	home = claudeQuotaAuth("home", now, 24*time.Hour, 0.01, 0.01)
	if pick("chat", "") != "other" {
		t.Fatal("do not repatriate warm chat")
	}
	other = claudeQuotaAuth("other", now, 24*time.Hour, 0.99, 0.99)
	home = claudeQuotaAuth("home", now, 24*time.Hour, 0.99, 0.99)
	if pick("chat", "") != "other" {
		t.Fatal("do not bounce between two constrained accounts")
	}
}

func TestAdaptiveReserveQueueMatchesSelectionAndCooldown(t *testing.T) {
	now := time.Now()
	selector := &AdaptiveReserveSelector{HomeAccount: "home"}
	m := NewManager(nil, selector, nil)
	m.SetConfig(&internalconfig.Config{Routing: internalconfig.RoutingConfig{Strategy: "adaptive-reserve", HomeAccount: "home"}})
	home := claudeQuotaAuth("home", now, 24*time.Hour, 0.5, 0.5)
	other := claudeQuotaAuth("other", now, 24*time.Hour, 0.1, 0.1)
	m.auths[home.ID] = home
	m.auths[other.ID] = other
	queue := m.RoutingQueue("claude", now)
	picked, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, m.List())
	if err != nil || queue[0].Auth != picked.ID || queue[0].Adaptive.Reason != "surplus_selected" {
		t.Fatalf("mismatch queue=%+v err=%v", queue, err)
	}
	other.Unavailable = true
	other.NextRetryAfter = now.Add(time.Hour)
	queue = m.RoutingQueue("claude", now)
	if queue[0].Auth != "home" || queue[1].Adaptive.Reason != "cooldown" {
		t.Fatal("cooldown must override surplus")
	}
}

func TestAdaptiveReserveUsageParserAndObservationIsolation(t *testing.T) {
	now := time.Now()
	valid := `{"member_dashboard_available":false,"limits":[],"weekly_scoped_shares":[],"five_hour":{"utilization":40,"resets_at":"2030-01-01T12:00:00Z"},"seven_day":{"utilization":50,"resets_at":"2030-01-04T12:00:00Z"}}`
	signals, ok := parseAdaptiveUsage(strings.NewReader(valid), now)
	if !ok || signals["Anthropic-Ratelimit-Unified-5h-Utilization"] != "0.400000" {
		t.Fatal("valid usage rejected")
	}
	for _, body := range []string{`{}`, `{"five_hour":{},"seven_day":null}`, `{"five_hour":{"utilization":101},"seven_day":null}`, `not json`} {
		if _, ok := parseAdaptiveUsage(strings.NewReader(body), now); ok {
			t.Fatalf("accepted invalid body %s", body)
		}
	}
	inactive, ok := parseAdaptiveUsage(strings.NewReader(`{"five_hour":null,"seven_day":null}`), now)
	if !ok || inactive["Adaptive-5h-Inactive"] != "true" {
		t.Fatal("inactive window must be explicit")
	}
	m := NewManager(nil, nil, nil)
	a := claudeQuotaAuth("home", now.Add(-time.Minute), time.Hour, 0.1, 0.1)
	a.Unavailable = true
	a.NextRetryAfter = now.Add(time.Hour)
	a.Quota.Exceeded = true
	m.auths[a.ID] = a
	source := a.Clone()
	m.applyAdaptiveQuota(source, signals, now)
	if !a.Unavailable || !a.Quota.Exceeded || !a.NextRetryAfter.After(now) {
		t.Fatal("poll cleared a real cooldown")
	}
	m.applyAdaptiveQuota(source, inactive, now.Add(-time.Minute))
	if a.Quota.Signals["Adaptive-5h-Inactive"] != "" {
		t.Fatal("stale poll overwrote newer observation")
	}
	source.CredentialVersion++
	m.applyAdaptiveQuota(source, inactive, now.Add(time.Minute))
	if a.Quota.Signals["Adaptive-5h-Inactive"] != "" {
		t.Fatal("old credential poll overwrote replacement")
	}
}

func TestAdaptiveReserveComparatorIsTransitive(t *testing.T) {
	var ranks []adaptiveRank
	for i := 0; i < 30; i++ {
		score := float64(i) / 29
		ranks = append(ranks, adaptiveRank{auth: &Auth{ID: string(rune('a' + i))}, decision: &AdaptiveDecision{Eligible: i%3 == 0, Home: i%7 == 0, Fresh: i%2 == 0, Score: score, Band: int(math.Floor(score / 0.1))}})
	}
	for _, a := range ranks {
		for _, b := range ranks {
			for _, c := range ranks {
				if adaptiveLess(a, b) && adaptiveLess(b, c) && !adaptiveLess(a, c) {
					t.Fatal("non-transitive ranking")
				}
			}
		}
	}
}

func TestAdaptiveReserveUnknownAndInvalidUsageIsNotFreeCapacity(t *testing.T) {
	now := time.Now()
	selector := &AdaptiveReserveSelector{HomeAccount: "home"}
	for _, value := range []string{"", "NaN", "-1", "2"} {
		a := claudeQuotaAuth("other", now, time.Hour, 0.1, 0.1)
		a.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Utilization"] = value
		if selector.decision(a, now).Eligible {
			t.Fatalf("invalid usage %q eligible", value)
		}
	}
	home := claudeQuotaAuth("home", now, time.Hour, 1, 1)
	other := claudeQuotaAuth("other", now, time.Hour, 0.95, 0.95)
	a, err := selector.Pick(context.Background(), "claude", "", cliproxyexecutor.Options{}, []*Auth{home, other})
	if err != nil || a.ID != "other" {
		t.Fatal("confirmed exhaustion must override home fallback")
	}
}

func TestAdaptiveReserveUsageTimestampJitterPreservesDemand(t *testing.T) {
	now := time.Now()
	points := demandPoints(now, false)
	original := buildDemandProfile(points, false, now)
	for i := range points {
		points[i].ShortReset += int64(i % 2)
	}
	jittered := buildDemandProfile(points, false, now)
	if original.recentUsed != jittered.recentUsed || original.recentHours != jittered.recentHours {
		t.Fatal("subsecond API reset rounding discarded real demand")
	}
}
