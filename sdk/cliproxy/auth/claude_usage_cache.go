package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Anthropic rate-limits /api/oauth/usage per token over a short window. The proxy's own
// poll is the only regular reader: it asks for the banked reset grants too (cedar_ember)
// and keeps the last good body per account, so the panel reads that copy instead of
// spending calls of its own. A forced read (panel refresh, reset preflight) goes through
// the same path, so there is never more than one request per account in flight.
const (
	claudeUsageURL          = "https://api.anthropic.com/api/oauth/usage?cedar_ember=1"
	claudeUsageForcedFloor  = 10 * time.Second
	claudeUsageBackoffBase  = 2 * time.Minute
	claudeUsageBackoffLimit = 16 * time.Minute
	claudeUsageMaxBody      = 1 << 20
)

// ClaudeUsageSnapshot is the last usage payload read for one Claude account.
type ClaudeUsageSnapshot struct {
	AuthID       string          `json:"auth_id"`
	AuthIndex    string          `json:"auth_index"`
	Body         json.RawMessage `json:"body,omitempty"`
	ObservedAt   *time.Time      `json:"observed_at,omitempty"`
	LastAttempt  *time.Time      `json:"last_attempt,omitempty"`
	LastStatus   int             `json:"last_status,omitempty"`
	BackoffUntil *time.Time      `json:"backoff_until,omitempty"`
}

type claudeUsageEntry struct {
	body         json.RawMessage
	observedAt   time.Time
	lastAttempt  time.Time
	lastStatus   int
	failures     int
	backoffUntil time.Time
}

type claudeUsageCache struct {
	mu       sync.Mutex
	entries  map[string]*claudeUsageEntry
	inflight map[string]chan struct{}
}

// begin claims the account's single read slot. It returns a channel that closes when an
// already running read finishes, or nil when the caller now owns the slot.
func (c *claudeUsageCache) begin(id string) <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.inflight == nil {
		c.inflight = make(map[string]chan struct{})
	}
	if ch, ok := c.inflight[id]; ok {
		return ch
	}
	c.inflight[id] = make(chan struct{})
	return nil
}

func (c *claudeUsageCache) end(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.inflight[id]; ok {
		close(ch)
		delete(c.inflight, id)
	}
}

func (c *claudeUsageCache) entry(id string) *claudeUsageEntry {
	if c.entries == nil {
		c.entries = make(map[string]*claudeUsageEntry)
	}
	e := c.entries[id]
	if e == nil {
		e = &claudeUsageEntry{}
		c.entries[id] = e
	}
	return e
}

// due reports whether a read should go upstream now. Scheduled polls respect the
// backoff after a 429; a forced read only waits out the short floor between attempts.
func (c *claudeUsageCache) due(id string, now time.Time, forced bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[id]
	if e == nil {
		return true
	}
	if forced {
		return now.Sub(e.lastAttempt) >= claudeUsageForcedFloor
	}
	return !now.Before(e.backoffUntil)
}

func (c *claudeUsageCache) record(id string, now time.Time, status int, body json.RawMessage, retryAfter time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entry(id)
	e.lastAttempt = now
	e.lastStatus = status
	switch {
	case status == http.StatusOK && body != nil:
		e.body = body
		e.observedAt = now
		e.failures = 0
		e.backoffUntil = time.Time{}
	case status == http.StatusTooManyRequests:
		e.failures++
		wait := claudeUsageBackoffBase
		for i := 1; i < e.failures && wait < claudeUsageBackoffLimit; i++ {
			wait *= 2
		}
		if wait > claudeUsageBackoffLimit {
			wait = claudeUsageBackoffLimit
		}
		if retryAfter > wait {
			wait = retryAfter
		}
		// Land just before the tick that follows the wait, so the next poll goes out.
		e.backoffUntil = now.Add(wait - 5*time.Second)
	}
}

func (c *claudeUsageCache) snapshot(a *Auth) ClaudeUsageSnapshot {
	out := ClaudeUsageSnapshot{AuthID: a.ID, AuthIndex: a.EnsureIndex()}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[a.ID]
	if e == nil {
		return out
	}
	out.Body = e.body
	out.LastStatus = e.lastStatus
	if !e.observedAt.IsZero() {
		t := e.observedAt
		out.ObservedAt = &t
	}
	if !e.lastAttempt.IsZero() {
		t := e.lastAttempt
		out.LastAttempt = &t
	}
	if !e.backoffUntil.IsZero() {
		t := e.backoffUntil
		out.BackoffUntil = &t
	}
	return out
}

func claudeUsageEligible(a *Auth) bool {
	return a != nil && !a.Disabled && strings.EqualFold(a.Provider, "claude") && a.AuthKind() != AuthKindAPIKey
}

// ClaudeUsageSnapshots returns the cached usage payload of every active Claude account.
func (m *Manager) ClaudeUsageSnapshots() []ClaudeUsageSnapshot {
	out := []ClaudeUsageSnapshot{}
	if m == nil {
		return out
	}
	for _, a := range m.List() {
		if claudeUsageEligible(a) {
			out = append(out, m.claudeUsage.snapshot(a))
		}
	}
	return out
}

// RefreshClaudeUsage reads the account's usage now, unless a read finished within the
// last few seconds or one is already running (then it waits for that one).
func (m *Manager) RefreshClaudeUsage(ctx context.Context, id string) (ClaudeUsageSnapshot, bool) {
	a, ok := m.GetByID(id)
	if !ok || !claudeUsageEligible(a) {
		return ClaudeUsageSnapshot{}, false
	}
	m.readClaudeUsage(ctx, a, true)
	return m.claudeUsage.snapshot(a), true
}

// readClaudeUsage performs at most one upstream usage read per account at a time and
// feeds a good answer to routing (applyAdaptiveQuota) and the cache.
func (m *Manager) readClaudeUsage(ctx context.Context, a *Auth, forced bool) {
	if wait := m.claudeUsage.begin(a.ID); wait != nil {
		if forced {
			select {
			case <-wait:
			case <-ctx.Done():
			}
		}
		return
	}
	defer m.claudeUsage.end(a.ID)
	if !m.claudeUsage.due(a.ID, time.Now(), forced) {
		return
	}
	m.pollAdaptiveQuota(ctx, a)
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return 0
}
