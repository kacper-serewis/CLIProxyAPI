package auth

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func claudeHistoryAuth(id string, longUsed float64, longReset time.Time) *Auth {
	return &Auth{ID: id, Provider: "claude", Quota: QuotaState{Signals: map[string]string{
		"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(longReset.Unix(), 10),
		"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(longUsed, 'f', -1, 64),
	}}}
}

func TestQuotaHistorySamplesOnlyChangesAndHeartbeats(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(48 * time.Hour)
	last := map[string]QuotaHistoryPoint{}

	if got := sampleQuotaHistory([]*Auth{claudeHistoryAuth("a", 0.2, reset)}, last, now); len(got) != 1 || got[0].LongUsed != 0.2 {
		t.Fatalf("first sample = %+v, want one point at 0.2", got)
	}
	if got := sampleQuotaHistory([]*Auth{claudeHistoryAuth("a", 0.2, reset)}, last, now.Add(10*time.Minute)); len(got) != 0 {
		t.Fatalf("unchanged sample recorded %+v", got)
	}
	if got := sampleQuotaHistory([]*Auth{claudeHistoryAuth("a", 0.25, reset)}, last, now.Add(20*time.Minute)); len(got) != 1 {
		t.Fatalf("changed sample not recorded")
	}
	if got := sampleQuotaHistory([]*Auth{claudeHistoryAuth("a", 0.25, reset)}, last, now.Add(90*time.Minute)); len(got) != 1 {
		t.Fatalf("heartbeat not recorded")
	}
}

func TestQuotaHistoryRecordsWhenObserved(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	reset := now.Add(48 * time.Hour)
	last := map[string]QuotaHistoryPoint{}
	observed := func(at time.Time) *Auth {
		auth := claudeHistoryAuth("a", 0.2, reset)
		auth.Quota.ObservedAt = at
		return auth
	}

	if got := sampleQuotaHistory([]*Auth{observed(now.Add(-time.Minute))}, last, now); len(got) != 1 || got[0].Observed != now.Add(-time.Minute).Unix() {
		t.Fatalf("first sample = %+v, want observed one minute ago", got)
	}
	if got := sampleQuotaHistory([]*Auth{observed(now.Add(4 * time.Minute))}, last, now.Add(5*time.Minute)); len(got) != 0 {
		t.Fatalf("recent observation recorded again: %+v", got)
	}
	if got := sampleQuotaHistory([]*Auth{observed(now.Add(10 * time.Minute))}, last, now.Add(11*time.Minute)); len(got) != 1 {
		t.Fatalf("observation 11 minutes later not recorded")
	}
	// No traffic: the hourly heartbeat keeps the old observation time.
	if got := sampleQuotaHistory([]*Auth{observed(now.Add(10 * time.Minute))}, last, now.Add(75*time.Minute)); len(got) != 1 || got[0].Observed != now.Add(10*time.Minute).Unix() {
		t.Fatalf("heartbeat = %+v, want the old observation time", got)
	}
}

func TestQuotaHistorySkipsRolledOverAndUnobserved(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	auths := []*Auth{claudeHistoryAuth("rolled", 0.9, now.Add(-time.Minute)), {ID: "fresh", Provider: "claude"}}
	if got := sampleQuotaHistory(auths, map[string]QuotaHistoryPoint{}, now); len(got) != 0 {
		t.Fatalf("recorded %+v, want nothing", got)
	}
}

func TestQuotaHistoryPersistsAndPrunes(t *testing.T) {
	path := filepath.Join(t.TempDir(), QuotaHistoryFileName)
	now := time.Now()
	appendQuotaHistory(path, []QuotaHistoryPoint{
		{At: now.Add(-40 * 24 * time.Hour).Unix(), Auth: "a", LongUsed: 0.1},
		{At: now.Add(-time.Hour).Unix(), Auth: "a", LongUsed: 0.3},
		{At: now.Unix(), Auth: "b", LongUsed: 0.5},
	})
	last := pruneQuotaHistory(path, now)
	if last["a"].LongUsed != 0.3 || last["b"].LongUsed != 0.5 {
		t.Fatalf("last = %+v", last)
	}
	points, err := ReadQuotaHistory(path, 0)
	if err != nil || len(points) != 2 {
		t.Fatalf("after prune: %d points, err %v", len(points), err)
	}
	if points, _ = ReadQuotaHistory(path, now.Unix()); len(points) != 1 || points[0].Auth != "b" {
		t.Fatalf("since filter returned %+v", points)
	}
}
