package auth

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	// soonestResetShortWindowSaturation demotes a credential whose short (5h) window is
	// getting full, so a new session does not bind to it only to fail over shortly after.
	// The credentials are shared with people who cannot switch accounts, so new sessions
	// leave headroom for them.
	soonestResetShortWindowSaturation = 0.80
	// soonestResetShortWindowDrain moves established sessions off a credential whose short
	// window is this full, before it hits the limit. Each moved session rebuilds its prompt
	// cache once on the new credential.
	soonestResetShortWindowDrain = 0.90
	// soonestResetLongWindowSaturation demotes a credential whose long (weekly) window is
	// effectively exhausted.
	soonestResetLongWindowSaturation = 0.97
	// soonestResetTieBucket groups long-window resets that are this close together so the
	// short-window utilization can break the tie.
	soonestResetTieBucket = time.Minute
)

// SoonestResetSelector picks the credential whose long (weekly) quota window resets
// soonest, so quota that would otherwise expire unused is spent first.
//
// Ranking, from most to least preferred:
//  1. Credentials with no current quota observation (never used, or their long window
//     already rolled over). One request is enough to learn their reset time.
//  2. Credentials with a known long-window reset, earliest reset first. Ties are broken
//     by the lower short-window utilization.
//  3. Credentials whose short or long window is saturated or rejected.
//
// Quota observations come from upstream response headers recorded on Auth.Quota.Signals
// (Anthropic unified rate-limit headers and Codex x-codex-* headers). When wrapped by
// SessionAffinitySelector this only decides cold bindings; established sessions keep
// their credential so prompt caches stay warm.
type SoonestResetSelector struct{}

type soonestResetRank struct {
	auth       *Auth
	index      int
	saturated  bool
	known      bool
	longReset  time.Time
	shortUsage float64
}

// Pick selects the available credential whose long quota window resets soonest.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	if len(available) == 1 {
		return available[0], nil
	}

	ranks := make([]soonestResetRank, len(available))
	for i, auth := range available {
		ranks[i] = rankSoonestReset(auth, i, now)
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		return soonestResetLess(ranks[i], ranks[j])
	})
	best := ranks[0]
	if entry := selectorLogEntry(ctx); entry != nil {
		reset := "unknown"
		if best.known {
			reset = best.longReset.UTC().Format(time.RFC3339)
		}
		entry.Debugf("soonest-reset: picked auth=%s provider=%s model=%s long_reset=%s saturated=%t candidates=%d", best.auth.ID, provider, model, reset, best.saturated, len(ranks))
	}
	return best.auth, nil
}

func soonestResetLess(a, b soonestResetRank) bool {
	if a.saturated != b.saturated {
		return !a.saturated
	}
	if a.known != b.known {
		return !a.known
	}
	if a.known {
		aBucket := a.longReset.Truncate(soonestResetTieBucket)
		bBucket := b.longReset.Truncate(soonestResetTieBucket)
		if !aBucket.Equal(bBucket) {
			return aBucket.Before(bBucket)
		}
	}
	if a.shortUsage != b.shortUsage {
		return a.shortUsage < b.shortUsage
	}
	return a.index < b.index
}

func rankSoonestReset(auth *Auth, index int, now time.Time) soonestResetRank {
	return rankSoonestResetWith(auth, index, now, soonestResetShortWindowSaturation)
}

// soonestResetDraining reports whether established sessions should leave the credential.
func soonestResetDraining(auth *Auth, now time.Time) bool {
	return rankSoonestResetWith(auth, 0, now, soonestResetShortWindowDrain).saturated
}

func rankSoonestResetWith(auth *Auth, index int, now time.Time, shortSaturation float64) soonestResetRank {
	rank := soonestResetRank{auth: auth, index: index}
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return rank
	}
	signals := auth.Quota.Signals
	var windows []quotaWindow
	if _, ok := signalValue(signals, "Anthropic-Ratelimit-Unified-7d-Reset"); ok {
		windows = claudeQuotaWindows(signals)
	} else {
		windows = codexQuotaWindows(signals, auth.Quota.ObservedAt)
	}

	var long, short *quotaWindow
	for i := range windows {
		w := &windows[i]
		if w.reset.IsZero() || !w.reset.After(now) {
			// The window already rolled over, so its observation no longer applies.
			continue
		}
		if w.long {
			long = w
		} else {
			short = w
		}
	}
	if long != nil {
		rank.known = true
		rank.longReset = long.reset
		if long.rejected || long.usage >= soonestResetLongWindowSaturation {
			rank.saturated = true
		}
	}
	if short != nil {
		rank.shortUsage = short.usage
		if short.rejected || short.usage >= shortSaturation {
			rank.saturated = true
		}
	}
	return rank
}

type quotaWindow struct {
	long     bool
	reset    time.Time
	usage    float64 // fraction in [0, 1]
	rejected bool
}

func claudeQuotaWindows(signals map[string]string) []quotaWindow {
	windows := make([]quotaWindow, 0, 2)
	for _, spec := range []struct {
		name string
		long bool
	}{{"5h", false}, {"7d", true}} {
		prefix := "Anthropic-Ratelimit-Unified-" + spec.name + "-"
		reset, ok := signalUnixTime(signals, prefix+"Reset")
		if !ok {
			continue
		}
		usage, _ := signalFloat(signals, prefix+"Utilization")
		status, _ := signalValue(signals, prefix+"Status")
		windows = append(windows, quotaWindow{
			long:     spec.long,
			reset:    reset,
			usage:    usage,
			rejected: strings.EqualFold(status, "rejected"),
		})
	}
	return windows
}

// codexQuotaWindows reads the primary and secondary Codex windows. The window with the
// larger duration is treated as the long window, whichever header slot carries it.
func codexQuotaWindows(signals map[string]string, observedAt time.Time) []quotaWindow {
	limitReached := false
	if value, ok := signalValue(signals, "X-Codex-Limit-Reached"); ok {
		limitReached, _ = strconv.ParseBool(value)
	}
	type codexWindow struct {
		window  quotaWindow
		minutes float64
	}
	found := make([]codexWindow, 0, 2)
	for _, slot := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + slot + "-"
		minutes, _ := signalFloat(signals, prefix+"Window-Minutes")
		if minutes <= 0 {
			continue
		}
		reset, ok := signalUnixTime(signals, prefix+"Reset-At")
		if !ok {
			seconds, okSeconds := signalFloat(signals, prefix+"Reset-After-Seconds")
			if !okSeconds || observedAt.IsZero() {
				continue
			}
			reset = observedAt.Add(time.Duration(seconds * float64(time.Second)))
		}
		usedPercent, _ := signalFloat(signals, prefix+"Used-Percent")
		found = append(found, codexWindow{
			window:  quotaWindow{reset: reset, usage: usedPercent / 100, rejected: limitReached && usedPercent >= 100},
			minutes: minutes,
		})
	}
	if len(found) == 0 {
		return nil
	}
	longest := 0
	for i := range found {
		if found[i].minutes > found[longest].minutes {
			longest = i
		}
	}
	windows := make([]quotaWindow, 0, len(found))
	for i := range found {
		w := found[i].window
		w.long = i == longest
		windows = append(windows, w)
	}
	return windows
}

func signalValue(signals map[string]string, name string) (string, bool) {
	if value, ok := signals[name]; ok {
		return strings.TrimSpace(value), true
	}
	for key, value := range signals {
		if strings.EqualFold(key, name) {
			return strings.TrimSpace(value), true
		}
	}
	return "", false
}

func signalFloat(signals map[string]string, name string) (float64, bool) {
	value, ok := signalValue(signals, name)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

func signalUnixTime(signals map[string]string, name string) (time.Time, bool) {
	seconds, ok := signalFloat(signals, name)
	if !ok || seconds <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(seconds), 0), true
}
