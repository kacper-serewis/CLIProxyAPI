package auth

import (
	"context"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// AdaptiveReserveSelector protects forecast demand, then consumes likely surplus.
// History includes ALL account usage: attribution to individual employees is not
// available. Treating our own traffic as demand is deliberately conservative.
type AdaptiveReserveSelector struct {
	HomeAccount string
	history     *quotaDemandHistory
}

type ReserveWindow struct {
	Inactive    bool      `json:"inactive"`
	Used        float64   `json:"used"`
	Reset       time.Time `json:"reset"`
	Reserve     float64   `json:"reserve"`
	Spare       float64   `json:"spare"`
	Forecast    float64   `json:"forecast"`
	Buffer      float64   `json:"buffer"`
	RecentRate  float64   `json:"recent_rate"`
	TypicalRate float64   `json:"typical_rate"`
	Learning    bool      `json:"learning"`
}

type AdaptiveDecision struct {
	Home       bool           `json:"home"`
	Fresh      bool           `json:"fresh"`
	ObservedAt time.Time      `json:"observed_at"`
	Eligible   bool           `json:"eligible"`
	Draining   bool           `json:"draining"`
	Score      float64        `json:"score"`
	Band       int            `json:"band"`
	Reason     string         `json:"reason"`
	Short      *ReserveWindow `json:"short,omitempty"`
	Long       *ReserveWindow `json:"long,omitempty"`
}

type quotaDemandHistory struct {
	sync.RWMutex
	points map[string][]QuotaHistoryPoint
}

func (h *quotaDemandHistory) add(points []QuotaHistoryPoint, now time.Time) {
	if h == nil {
		return
	}
	h.Lock()
	defer h.Unlock()
	if h.points == nil {
		h.points = make(map[string][]QuotaHistoryPoint)
	}
	cutoff := now.Add(-quotaHistoryRetention).Unix()
	for _, p := range points {
		if p.Provider != "claude" || p.Observed <= 0 || p.Observed < cutoff || p.Observed > now.Add(time.Minute).Unix() {
			continue
		}
		list := h.points[p.Auth]
		// Heartbeats of the same observation are not evidence that an account is quiet.
		if len(list) > 0 && p.Observed <= list[len(list)-1].Observed {
			continue
		}
		list = append(list, p)
		first := sort.Search(len(list), func(i int) bool { return list[i].Observed >= cutoff })
		h.points[p.Auth] = append([]QuotaHistoryPoint(nil), list[first:]...)
	}
}

func (h *quotaDemandHistory) snapshot(id string) []QuotaHistoryPoint {
	if h == nil {
		return nil
	}
	h.RLock()
	defer h.RUnlock()
	return append([]QuotaHistoryPoint(nil), h.points[id]...)
}

func attachAdaptiveHistory(selector Selector, history *quotaDemandHistory) {
	switch s := selector.(type) {
	case *AdaptiveReserveSelector:
		s.history = history
	case *SessionAffinitySelector:
		attachAdaptiveHistory(s.fallback, history)
	}
}

func (s *AdaptiveReserveSelector) isHome(a *Auth) bool {
	home := strings.TrimSpace(s.HomeAccount)
	if home == "" || a == nil {
		return false
	}
	if strings.EqualFold(a.ID, home) {
		return true
	}
	email, _ := a.Metadata["email"].(string)
	return strings.EqualFold(email, home)
}

// A profile is computed once per window, not once per future hour. Hours are
// interpreted in the proxy's local timezone, separating weekdays from weekends.
type demandProfile struct {
	used, hours             [2][24]float64
	recentUsed, recentHours float64
}

func dayKind(t time.Time) int {
	if t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		return 1
	}
	return 0
}
func buildDemandProfile(points []QuotaHistoryPoint, long bool, now time.Time) demandProfile {
	p := demandProfile{}
	for i := 1; i < len(points); i++ {
		a, b := points[i-1], points[i]
		dt := float64(b.Observed-a.Observed) / 3600
		if dt <= 0 || dt > 0.5 || b.Observed > now.Unix() {
			continue
		}
		au, bu, ar, br := a.ShortUsed, b.ShortUsed, a.ShortReset, b.ShortReset
		if long {
			au, bu, ar, br = a.LongUsed, b.LongUsed, a.LongReset, b.LongReset
		}
		if ar == 0 || ar != br || bu < au || b.Observed >= br {
			continue
		}
		delta := bu - au
		if b.Observed > now.Add(-time.Hour).Unix() {
			fraction := math.Min(1, float64(b.Observed-now.Add(-time.Hour).Unix())/(dt*3600))
			p.recentUsed += delta * fraction
			p.recentHours += dt * fraction
		}
		at := time.Unix((a.Observed+b.Observed)/2, 0).In(now.Location())
		day, hour := dayKind(at), at.Hour()
		p.used[day][hour] += delta
		p.hours[day][hour] += dt
	}
	return p
}
func (p demandProfile) typical(at time.Time, baseline float64) (float64, bool) {
	day, hour := dayKind(at), at.Hour()
	if p.hours[day][hour] < 0.5 {
		return baseline, false
	}
	return p.used[day][hour] / p.hours[day][hour], true
}

func forecastWindow(w quotaWindow, points []QuotaHistoryPoint, now time.Time) *ReserveWindow {
	remaining := w.reset.Sub(now).Hours()
	// Bound forecast work even if an upstream timestamp is malformed.
	horizon := 5.0
	if w.long {
		horizon = 7 * 24
	}
	remaining = math.Min(remaining, horizon)
	if remaining <= 0 {
		return nil
	}
	baseline, buffer, taper := 0.04, 0.05, 1.0
	if w.long {
		baseline, buffer, taper = 0.003, 0.03, 24
	}
	profile := buildDemandProfile(points, w.long, now)
	recent := 0.0
	if profile.recentHours > 0 {
		recent = profile.recentUsed / profile.recentHours
	}
	typical, learnedTypical := profile.typical(now, baseline)
	learnedRecent := profile.recentHours >= 0.5
	activity := 1.0
	if learnedRecent {
		activity = math.Max(0.15, math.Min(2, recent/math.Max(typical, 0.001)))
	}
	forecast := 0.0
	learning := !learnedRecent || !learnedTypical
	// Integrate hourly patterns, retaining a demand floor even on a quiet day.
	for elapsed := 0.0; elapsed < remaining; {
		step := math.Min(1, remaining-elapsed)
		at := now.Add(time.Duration(elapsed * float64(time.Hour)))
		rate, learned := profile.typical(at, baseline)
		if !learned {
			learning = true
		}
		if w.long {
			// Today's absence matters most today; do not extrapolate it over the entire week.
			weight := math.Exp(-elapsed / 24)
			rate *= 1 + (activity-1)*weight
		} else if learnedRecent {
			rate = math.Max(recent, rate*0.25)
		}
		forecast += rate * step
		elapsed += step
	}
	buffer *= math.Min(1, remaining/taper)
	reserve := math.Min(0.95, forecast+buffer)
	return &ReserveWindow{Used: w.usage, Reset: w.reset, Forecast: forecast, Buffer: buffer, Reserve: reserve, Spare: 1 - w.usage - reserve, RecentRate: recent, TypicalRate: typical, Learning: learning}
}

func (s *AdaptiveReserveSelector) decision(a *Auth, now time.Time) *AdaptiveDecision {
	d := &AdaptiveDecision{Home: s.isHome(a), Reason: "unknown"}
	if a == nil {
		return d
	}
	d.ObservedAt = a.Quota.ObservedAt
	age := now.Sub(a.Quota.ObservedAt)
	d.Fresh = !a.Quota.ObservedAt.IsZero() && age >= -time.Minute && age <= 10*time.Minute
	points := s.history.snapshot(a.ID)
	if p, ok := quotaHistoryPointFor(a, now); ok && (len(points) == 0 || p.Observed > points[len(points)-1].Observed) {
		points = append(points, p)
	}
	rejected := false
	for _, w := range claudeQuotaWindows(a.Quota.Signals) {
		name := "5h"
		if w.long {
			name = "7d"
		}
		usage, valid := signalFloat(a.Quota.Signals, "Anthropic-Ratelimit-Unified-"+name+"-Utilization")
		if !valid || usage < 0 || usage > 1 {
			continue
		}
		f := forecastWindow(w, points, now)
		if f == nil {
			continue
		}
		f.Inactive = a.Quota.Signals["Adaptive-"+name+"-Inactive"] == "true"
		rejected = rejected || w.rejected || w.usage >= 1
		if w.long {
			d.Long = f
		} else {
			d.Short = f
		}
	}
	if d.Short == nil || d.Long == nil {
		return d
	}
	d.Draining = rejected || d.Short.Spare <= 0 || d.Long.Spare <= 0
	d.Eligible = d.Fresh && !rejected && d.Short.Spare >= 0.05 && d.Long.Spare >= 0.02
	shortSpare, longSpare := math.Max(0, d.Short.Spare), math.Max(0, d.Long.Spare)
	urgency := math.Max(shortSpare/(1+d.Short.Reset.Sub(now).Hours()), longSpare/(1+d.Long.Reset.Sub(now).Hours()/24))
	d.Score = math.Min(shortSpare, longSpare) + 0.25*urgency
	d.Band = int(math.Floor(d.Score / 0.1))
	switch {
	case rejected:
		d.Reason = "exhausted"
	case !d.Fresh:
		d.Reason = "stale"
	case !d.Eligible:
		d.Reason = "reserved"
	default:
		d.Reason = "surplus"
	}
	return d
}

type adaptiveRank struct {
	auth     *Auth
	decision *AdaptiveDecision
}

func adaptiveLess(a, b adaptiveRank) bool {
	x, y := a.decision, b.decision
	if x.Eligible != y.Eligible {
		return x.Eligible
	}
	// If everyone is under pressure, use home first rather than spending colleagues' reserves.
	if !x.Eligible {
		if (x.Reason == "exhausted") != (y.Reason == "exhausted") {
			return x.Reason != "exhausted"
		}
		if x.Home != y.Home {
			return x.Home
		}
		if x.Fresh != y.Fresh {
			return x.Fresh
		}
	}
	if x.Band != y.Band {
		return x.Band > y.Band
	}
	if x.Home != y.Home {
		return x.Home
	}
	if x.Score != y.Score {
		return x.Score > y.Score
	}
	return a.auth.ID < b.auth.ID
}

func (s *AdaptiveReserveSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	if !strings.EqualFold(provider, "claude") {
		return (&SoonestResetSelector{}).Pick(ctx, provider, model, opts, auths)
	}
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	ranks := make([]adaptiveRank, 0, len(available))
	for _, a := range available {
		ranks = append(ranks, adaptiveRank{a, s.decision(a, now)})
	}
	sort.SliceStable(ranks, func(i, j int) bool { return adaptiveLess(ranks[i], ranks[j]) })
	best := ranks[0]
	if entry := selectorLogEntry(ctx); entry != nil {
		entry.Debugf("adaptive-reserve: picked auth=%s home=%t eligible=%t reason=%s score=%.3f candidates=%d", best.auth.ID, best.decision.Home, best.decision.Eligible, best.decision.Reason, best.decision.Score, len(ranks))
	}
	return best.auth, nil
}
