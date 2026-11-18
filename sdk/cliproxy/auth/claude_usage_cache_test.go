package auth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

type usageStubExecutor struct {
	calls  atomic.Int32
	status atomic.Int32
	urls   sync.Map
	gate   chan struct{}
}

func (e *usageStubExecutor) Identifier() string { return "claude" }
func (e *usageStubExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *usageStubExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}
func (e *usageStubExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) { return a, nil }
func (e *usageStubExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}
func (e *usageStubExecutor) HttpRequest(_ context.Context, _ *Auth, req *http.Request) (*http.Response, error) {
	e.calls.Add(1)
	e.urls.Store(req.URL.String(), true)
	if e.gate != nil {
		<-e.gate
	}
	status := int(e.status.Load())
	if status == 0 {
		status = http.StatusOK
	}
	body := `{"error":{"type":"rate_limit_error"}}`
	if status == http.StatusOK {
		body = `{"five_hour":{"utilization":40,"resets_at":"2030-01-01T05:00:00Z"},"seven_day":{"utilization":12,"resets_at":"2030-01-07T00:00:00Z"},"cedar_ember":{"grants":[]}}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"0"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func newUsageTestManager(t *testing.T, exec *usageStubExecutor) (*Manager, *Auth) {
	t.Helper()
	m := NewManager(nil, nil, nil)
	m.RegisterExecutor(exec)
	a, err := m.Register(context.Background(), &Auth{ID: "claude-a.json", Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	return m, a
}

func TestClaudeUsageCacheKeepsBodyAndAsksForGrants(t *testing.T) {
	exec := &usageStubExecutor{}
	m, a := newUsageTestManager(t, exec)
	m.readClaudeUsage(context.Background(), a, false)
	if _, ok := exec.urls.Load(claudeUsageURL); !ok || !strings.Contains(claudeUsageURL, "cedar_ember=1") {
		t.Fatalf("poll did not ask for reset grants")
	}
	snaps := m.ClaudeUsageSnapshots()
	if len(snaps) != 1 || snaps[0].ObservedAt == nil || !strings.Contains(string(snaps[0].Body), "cedar_ember") {
		t.Fatalf("snapshot = %+v", snaps)
	}
	if snaps[0].AuthIndex == "" {
		t.Fatalf("snapshot has no auth index")
	}
	// A second scheduled poll right away still goes out (no backoff after success),
	// but a forced read within the floor reuses the fresh copy.
	m.readClaudeUsage(context.Background(), a, true)
	if got := exec.calls.Load(); got != 1 {
		t.Fatalf("forced read within floor made %d calls, want 1", got)
	}
}

func TestClaudeUsageBacksOffAfter429AndKeepsLastBody(t *testing.T) {
	exec := &usageStubExecutor{}
	m, a := newUsageTestManager(t, exec)
	m.readClaudeUsage(context.Background(), a, false)
	exec.status.Store(http.StatusTooManyRequests)
	m.claudeUsage.mu.Lock()
	m.claudeUsage.entries[a.ID].lastAttempt = time.Now().Add(-time.Minute)
	m.claudeUsage.mu.Unlock()
	m.readClaudeUsage(context.Background(), a, false)
	m.readClaudeUsage(context.Background(), a, false)
	if got := exec.calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2 (third poll should wait out the backoff)", got)
	}
	snap := m.ClaudeUsageSnapshots()[0]
	if snap.LastStatus != http.StatusTooManyRequests || snap.BackoffUntil == nil || snap.Body == nil {
		t.Fatalf("snapshot after 429 = %+v", snap)
	}
	if d := time.Until(*snap.BackoffUntil); d < time.Minute || d > 2*time.Minute {
		t.Fatalf("first backoff = %v, want just under 2m", d)
	}
}

func TestClaudeUsageBackoffDoubles(t *testing.T) {
	var c claudeUsageCache
	now := time.Now()
	for i, want := range []time.Duration{2, 4, 8, 16, 16} {
		c.record("x", now, http.StatusTooManyRequests, nil, 0)
		if got := c.entries["x"].backoffUntil.Sub(now) + 5*time.Second; got != want*time.Minute {
			t.Fatalf("failure %d: backoff %v, want %vm", i+1, got, want)
		}
	}
	c.record("x", now, http.StatusOK, []byte(`{}`), 0)
	if !c.entries["x"].backoffUntil.IsZero() || c.entries["x"].failures != 0 {
		t.Fatalf("success did not clear the backoff")
	}
}

func TestClaudeUsageForcedReadJoinsRunningRead(t *testing.T) {
	exec := &usageStubExecutor{gate: make(chan struct{})}
	m, a := newUsageTestManager(t, exec)
	done := make(chan struct{})
	go func() { m.readClaudeUsage(context.Background(), a, false); close(done) }()
	for exec.calls.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	joined := make(chan ClaudeUsageSnapshot)
	go func() {
		snap, _ := m.RefreshClaudeUsage(context.Background(), a.ID)
		joined <- snap
	}()
	time.Sleep(20 * time.Millisecond)
	close(exec.gate)
	snap := <-joined
	<-done
	if exec.calls.Load() != 1 || snap.ObservedAt == nil {
		t.Fatalf("calls = %d, snapshot = %+v", exec.calls.Load(), snap)
	}
}
