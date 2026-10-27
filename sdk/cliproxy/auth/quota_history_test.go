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
