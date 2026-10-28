package auth

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	log "github.com/sirupsen/logrus"
)

// Reads usage, never generates messages or consumes inference quota. One in-flight
// request per account; an unresponsive account cannot block the others. The service
// context cancels requests on shutdown. Failures leave observations to become stale,
// and never change credential cooldown, refresh, or session-affinity state.
func (m *Manager) startAdaptiveQuotaPolling(ctx context.Context) {
	go func() {
		var pending sync.Map
		ticker := time.NewTicker(quotaHistorySampleInterval)
		defer ticker.Stop()
		for {
			cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
			if cfg != nil && strings.EqualFold(strings.TrimSpace(cfg.Routing.Strategy), "adaptive-reserve") {
				for _, a := range m.List() {
					if a.Disabled || !strings.EqualFold(a.Provider, "claude") || a.AuthKind() == AuthKindAPIKey {
						continue
					}
					if _, loaded := pending.LoadOrStore(a.ID, true); loaded {
						continue
					}
					go func(a *Auth) {
						defer pending.Delete(a.ID)
						m.pollAdaptiveQuota(ctx, a)
					}(a)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (m *Manager) pollAdaptiveQuota(ctx context.Context, a *Auth) {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/api/oauth/usage", nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", "claude-cli/2.1.310 (external, cli)")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	resp, err := m.HttpRequest(ctx, a, req)
	if err != nil {
		// Do not log arbitrary upstream errors or bodies: they can contain credentials.
		if ctx.Err() == nil {
			log.Warnf("adaptive-reserve: quota read failed auth=%s", a.ID)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.Warnf("adaptive-reserve: quota read auth=%s status=%d", a.ID, resp.StatusCode)
		return
	}
	signals, ok := parseAdaptiveUsage(io.LimitReader(resp.Body, 1<<20), started)
	if !ok {
		log.Warnf("adaptive-reserve: invalid quota response auth=%s", a.ID)
		return
	}
	m.applyAdaptiveQuota(a, signals, started)
}

func parseAdaptiveUsage(reader io.Reader, now time.Time) (map[string]string, bool) {
	type window struct {
		Utilization *float64   `json:"utilization"`
		ResetsAt    *time.Time `json:"resets_at"`
	}
	var payload map[string]json.RawMessage
	if json.NewDecoder(reader).Decode(&payload) != nil {
		return nil, false
	}
	signals := make(map[string]string)
	for _, spec := range []struct {
		key, name string
		duration  time.Duration
	}{{"five_hour", "5h", 5 * time.Hour}, {"seven_day", "7d", 7 * 24 * time.Hour}} {
		raw, present := payload[spec.key]
		var w *window
		if !present || json.Unmarshal(raw, &w) != nil {
			return nil, false
		}
		used, reset := 0.0, now.Add(spec.duration)
		if w != nil {
			if w.Utilization == nil || math.IsNaN(*w.Utilization) || *w.Utilization < 0 || *w.Utilization > 100 {
				return nil, false
			}
			used = *w.Utilization / 100
			if w.ResetsAt != nil {
				reset = *w.ResetsAt
			} else if used != 0 {
				return nil, false
			}
		}
		prefix := "Anthropic-Ratelimit-Unified-" + spec.name + "-"
		signals[prefix+"Utilization"] = strconv.FormatFloat(used, 'f', 6, 64)
		signals[prefix+"Reset"] = strconv.FormatInt(reset.Unix(), 10)
		signals[prefix+"Status"] = "allowed"
		if used >= 1 {
			signals[prefix+"Status"] = "rejected"
		}
		// A null window has not started. Use a full planning horizon, never imply an
		// imminent reset. The dashboard labels this explicitly; history omits its reset.
		if w == nil || w.ResetsAt == nil {
			signals["Adaptive-"+spec.name+"-Inactive"] = "true"
		}
	}
	return signals, true
}

func (m *Manager) applyAdaptiveQuota(source *Auth, signals map[string]string, observed time.Time) {
	m.mu.Lock()
	a := m.auths[source.ID]
	if a == nil || a.Disabled || a.CredentialVersion != source.CredentialVersion || a.RegistrationEpoch != source.RegistrationEpoch || a.Quota.ObservedAt.After(observed) {
		m.mu.Unlock()
		return
	}
	// Only replace observation fields; a usage poll must never clear a real 429.
	a.Quota.Signals = signals
	a.Quota.ObservedAt = observed
	snapshot := a.Clone()
	m.mu.Unlock()
	if m.scheduler != nil {
		m.scheduler.upsertAuth(snapshot)
	}
	if point, ok := quotaHistoryPointFor(snapshot, observed); ok {
		m.adaptiveHistory.add([]QuotaHistoryPoint{point}, observed)
	}
}
