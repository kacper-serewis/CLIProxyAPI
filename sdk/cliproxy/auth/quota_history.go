package auth

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// QuotaHistoryFileName is stored in the auth directory. The .jsonl suffix keeps the
	// auth watcher, which only loads *.json, from treating it as a credential.
	QuotaHistoryFileName = "quota-history.jsonl"

	quotaHistorySampleInterval = 2 * time.Minute
	// quotaHistoryHeartbeat records an unchanged observation so charts can tell
	// "still the same" from "no data".
	quotaHistoryHeartbeat = time.Hour
	// quotaHistoryObservedStep records unchanged values again once the provider has
	// reported them this much later, so readers can tell how fresh the values are.
	quotaHistoryObservedStep = 10 * time.Minute
	quotaHistoryRetention    = 30 * 24 * time.Hour
)

// QuotaHistoryPoint is one credential's quota observation. Usage values are fractions in
// [0, 1]; reset values are unix seconds. The long window is Claude's 7d window or the
// longer Codex window; the short window is Claude's 5h window or the shorter Codex one.
// Observed is when the provider last reported these values (unix seconds; the quota only
// updates from proxy traffic or adaptive usage polling), At is when the point was recorded.
type QuotaHistoryPoint struct {
	At         int64   `json:"t"`
	Observed   int64   `json:"observed,omitempty"`
	Auth       string  `json:"auth"`
	Provider   string  `json:"provider"`
	LongUsed   float64 `json:"long_used"`
	LongReset  int64   `json:"long_reset"`
	ShortUsed  float64 `json:"short_used,omitempty"`
	ShortReset int64   `json:"short_reset,omitempty"`
}

// QuotaHistoryPath returns the history file location for an auth directory.
func QuotaHistoryPath(authDir string) string {
	return filepath.Join(authDir, QuotaHistoryFileName)
}

var quotaHistoryFileMu sync.Mutex

// StartQuotaHistory periodically appends changed quota observations to path until ctx ends.
func (m *Manager) StartQuotaHistory(ctx context.Context, path string) {
	if m == nil || strings.TrimSpace(path) == "" {
		return
	}
	go func() {
		last := pruneQuotaHistory(path, time.Now())
		history, _ := ReadQuotaHistory(path, time.Now().Add(-quotaHistoryRetention).Unix())
		m.adaptiveHistory.add(history, time.Now())
		m.startAdaptiveQuotaPolling(ctx)
		ticker := time.NewTicker(quotaHistorySampleInterval)
		defer ticker.Stop()
		for {
			points := sampleQuotaHistory(m.List(), last, time.Now())
			m.adaptiveHistory.add(points, time.Now())
			appendQuotaHistory(path, points)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// sampleQuotaHistory returns the points that differ from, were observed 10 minutes later
// than, or are an hour newer than the last recorded point per credential, and updates last
// accordingly.
func sampleQuotaHistory(auths []*Auth, last map[string]QuotaHistoryPoint, now time.Time) []QuotaHistoryPoint {
	var points []QuotaHistoryPoint
	for _, auth := range auths {
		point, ok := quotaHistoryPointFor(auth, now)
		if !ok {
			continue
		}
		if prev, seen := last[point.Auth]; seen && sameQuotaObservation(prev, point) &&
			now.Unix()-prev.At < int64(quotaHistoryHeartbeat/time.Second) &&
			point.Observed-prev.Observed < int64(quotaHistoryObservedStep/time.Second) {
			continue
		}
		last[point.Auth] = point
		points = append(points, point)
	}
	return points
}

func quotaHistoryPointFor(auth *Auth, now time.Time) (QuotaHistoryPoint, bool) {
	if auth == nil || auth.Disabled || len(auth.Quota.Signals) == 0 {
		return QuotaHistoryPoint{}, false
	}
	var windows []quotaWindow
	if _, ok := signalValue(auth.Quota.Signals, "Anthropic-Ratelimit-Unified-7d-Reset"); ok {
		windows = claudeQuotaWindows(auth.Quota.Signals)
	} else {
		windows = codexQuotaWindows(auth.Quota.Signals, auth.Quota.ObservedAt)
	}
	point := QuotaHistoryPoint{At: now.Unix(), Auth: auth.ID, Provider: strings.ToLower(auth.Provider)}
	if !auth.Quota.ObservedAt.IsZero() {
		point.Observed = auth.Quota.ObservedAt.Unix()
	}
	hasLong := false
	for _, w := range windows {
		// A rolled-over window no longer describes current usage.
		if w.reset.IsZero() || !w.reset.After(now) {
			continue
		}
		if w.long {
			hasLong = true
			point.LongUsed = roundQuotaUsage(w.usage)
			point.LongReset = w.reset.Unix()
		} else {
			point.ShortUsed = roundQuotaUsage(w.usage)
			point.ShortReset = w.reset.Unix()
		}
	}
	if auth.Quota.Signals["Adaptive-5h-Inactive"] == "true" {
		point.ShortReset = 0
	}
	if auth.Quota.Signals["Adaptive-7d-Inactive"] == "true" {
		point.LongReset = 0
	}
	return point, hasLong
}

func roundQuotaUsage(usage float64) float64 {
	return math.Round(math.Min(math.Max(usage, 0), 1)*10000) / 10000
}

func sameQuotaObservation(a, b QuotaHistoryPoint) bool {
	return a.LongUsed == b.LongUsed && a.LongReset == b.LongReset && a.ShortUsed == b.ShortUsed && a.ShortReset == b.ShortReset
}

func appendQuotaHistory(path string, points []QuotaHistoryPoint) {
	if len(points) == 0 {
		return
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, point := range points {
		_ = enc.Encode(point)
	}
	quotaHistoryFileMu.Lock()
	defer quotaHistoryFileMu.Unlock()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.WithError(err).Warn("quota history: open failed")
		return
	}
	defer func() { _ = f.Close() }()
	if _, err = f.Write(buf.Bytes()); err != nil {
		log.WithError(err).Warn("quota history: write failed")
	}
}

// pruneQuotaHistory drops points past retention and returns the newest point per credential.
func pruneQuotaHistory(path string, now time.Time) map[string]QuotaHistoryPoint {
	cutoff := now.Add(-quotaHistoryRetention).Unix()
	all, _ := ReadQuotaHistory(path, 0)
	last := make(map[string]QuotaHistoryPoint)
	kept := all[:0]
	for _, point := range all {
		if point.At < cutoff {
			continue
		}
		kept = append(kept, point)
		last[point.Auth] = point
	}
	if len(kept) < len(all) {
		quotaHistoryFileMu.Lock()
		tmp := path + ".tmp"
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for _, point := range kept {
			_ = enc.Encode(point)
		}
		if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err == nil {
			_ = os.Rename(tmp, path)
		}
		quotaHistoryFileMu.Unlock()
	}
	return last
}

// ReadQuotaHistory returns recorded points at or after since (unix seconds), oldest first.
// A missing file yields no points.
func ReadQuotaHistory(path string, since int64) ([]QuotaHistoryPoint, error) {
	quotaHistoryFileMu.Lock()
	defer quotaHistoryFileMu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var points []QuotaHistoryPoint
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var point QuotaHistoryPoint
		if json.Unmarshal(scanner.Bytes(), &point) != nil || point.At < since {
			continue
		}
		points = append(points, point)
	}
	sort.SliceStable(points, func(i, j int) bool { return points[i].At < points[j].At })
	return points, scanner.Err()
}
