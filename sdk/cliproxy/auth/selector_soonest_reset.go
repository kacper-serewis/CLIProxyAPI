package auth

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

const (
	// soonestResetShortWindowSaturation demotes a credential whose short (5h) window is
	// nearly full, so a new session does not bind to it only to fail over shortly after.
	// The credentials are used by one person, so little headroom is kept back.
	soonestResetShortWindowSaturation = 0.90
	// soonestResetShortWindowDrain moves established sessions off a credential whose short
	// window is this full, before it hits the limit, when a safe alternative exists. Each
	// moved session rebuilds its prompt cache once on the new credential.
	soonestResetShortWindowDrain = 0.95
	// soonestResetLongWindowSaturation demotes a credential whose long (weekly) window is
	// effectively exhausted.
	soonestResetLongWindowSaturation = 0.99
	// soonestResetLongWindowReserve keeps the last stretch of a weekly window back: from
	// this usage on, new sessions go to another credential while one has room, and come
	// here only when the others are saturated. A full short window costs about 15 weekly
	// points, so this leaves one short window of overflow for when the other credential
	// hits its 5h limit. Without it the soonest-reset credential ran dry well before its
	// reset and a busy afternoon had to fit in a single 5h window. Established sessions
	// stay, so their prompt caches survive.
	soonestResetLongWindowReserve = 0.84
	// soonestResetReserveCover releases the reserve once the usage expected before the
	// weekly reset is at most this many times what is left, so it is spent instead of
	// expiring. A fixed lead time wasted quota whenever the reset followed quiet hours.
	// Held on that line the credential takes 1/cover of the new sessions and runs out at
	// its reset; when usage falls short of the forecast it takes all of them.
	soonestResetReserveCover = 2.0
	// soonestResetDemandMinHours is the history a credential needs before its usage rhythm
	// is trusted. Without a forecast no reserve is kept.
	soonestResetDemandMinHours = 48.0
	// soonestResetDemandWindow is how far back the usage rhythm looks.
	soonestResetDemandWindow = 14 * 24 * time.Hour
	// soonestResetDemandTTL bounds how often the usage rhythm is rebuilt from the history.
	soonestResetDemandTTL = 2 * time.Minute
	// claudeFableSignalPrefix names the Fable-only weekly window in the quota signals. Each
	// seat has one next to its week, with the same reset; it fills about twice as fast as
	// the week when only Fable is used, and a seat that is out of it still serves Opus.
	claudeFableSignalPrefix = "Anthropic-Ratelimit-Unified-7d_oi-"
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
//  3. Credentials in their weekly reserve (see soonestResetLongWindowReserve), kept for
//     overflow from the ones above.
//  4. Credentials whose short or long window is saturated or rejected. For a Fable model
//     that includes the Fable-only weekly window, which also has its own reserve.
//
// Quota observations come from upstream response headers recorded on Auth.Quota.Signals
// (Anthropic unified rate-limit headers and Codex x-codex-* headers). When wrapped by
// SessionAffinitySelector this only decides cold bindings; established sessions keep
// their credential so prompt caches stay warm.
type SoonestResetSelector struct {
	// history feeds the usage forecast behind the weekly reserve; nil keeps no reserve.
	history *quotaDemandHistory

	demandMu sync.Mutex
	demands  map[string]soonestResetDemandEntry
}

type soonestResetDemandEntry struct {
	at     time.Time
	demand *soonestResetDemand
}

type soonestResetRank struct {
	auth       *Auth
	index      int
	saturated  bool
	reserved   bool
	known      bool
	longReset  time.Time
	longUsage  float64
	shortUsage float64
	// Set for a Fable model on a credential that reports its Fable-only weekly window.
	fableKnown bool
	fableReset time.Time
	fableUsage float64
}

// carryClaudeFableWindow keeps the last reading of the Fable-only weekly window when a new
// observation does not report it: only responses to Fable requests and the usage poll do,
// while every observation replaces the whole snapshot. The reading lapses at its reset.
func carryClaudeFableWindow(previous, next map[string]string, now time.Time) {
	if next == nil {
		return
	}
	if _, reported := signalValue(next, claudeFableSignalPrefix+"Reset"); reported {
		return
	}
	if window, ok := claudeFableWindow(previous); !ok || !window.reset.After(now) {
		return
	}
	for _, name := range []string{"Reset", "Utilization", "Status"} {
		if value, ok := signalValue(previous, claudeFableSignalPrefix+name); ok {
			next[claudeFableSignalPrefix+name] = value
		}
	}
}

// soonestResetUsesFableWindow reports whether requests for the model count against the
// Fable-only weekly window.
func soonestResetUsesFableWindow(model string) bool {
	return strings.Contains(strings.ToLower(model), "fable")
}

// claudeFableWindow reads the Fable-only weekly window from the quota signals.
func claudeFableWindow(signals map[string]string) (quotaWindow, bool) {
	reset, ok := signalUnixTime(signals, claudeFableSignalPrefix+"Reset")
	if !ok {
		return quotaWindow{}, false
	}
	usage, _ := signalFloat(signals, claudeFableSignalPrefix+"Utilization")
	status, _ := signalValue(signals, claudeFableSignalPrefix+"Status")
	return quotaWindow{long: true, reset: reset, usage: usage, rejected: strings.EqualFold(status, "rejected")}, true
}

// soonestResetDemand is the usage rhythm of a set of credentials: the share of one weekly
// window they burn together per hour, by weekday or weekend and local hour.
type soonestResetDemand struct {
	rate [2][24]float64
}

// newSoonestResetDemand learns the rhythm from the last soonestResetDemandWindow of quota
// history, the way the panel and menu bar do: only new highs within a weekly window count
// as usage (readings jitter), and time without samples counts as idle. With fable it is
// the rhythm of the Fable-only weekly window. It returns nil until one of the credentials
// has enough history.
func newSoonestResetDemand(history *quotaDemandHistory, auths []*Auth, now time.Time, fable bool) *soonestResetDemand {
	if history == nil {
		return nil
	}
	demand := &soonestResetDemand{}
	learned := false
	since := now.Add(-soonestResetDemandWindow).Unix()
	for _, auth := range auths {
		if auth == nil || auth.Disabled {
			continue
		}
		type sample struct {
			observed, reset int64
			used            float64
		}
		var samples []sample
		for _, point := range history.snapshot(auth.ID) {
			item := sample{point.Observed, point.LongReset, point.LongUsed}
			if fable {
				item = sample{point.Observed, point.FableReset, point.FableUsed}
			}
			if item.reset != 0 && item.observed >= since && item.observed <= now.Unix() {
				samples = append(samples, item)
			}
		}
		if len(samples) < 2 {
			continue
		}
		first := time.Unix(samples[0].observed, 0).In(now.Location())
		if now.Sub(first).Hours() < soonestResetDemandMinHours {
			continue
		}
		learned = true
		var used, hours [2][24]float64
		high, window := samples[0].used, samples[0].reset
		for _, item := range samples[1:] {
			// Reset timestamps jitter by a second or two within the same window.
			if math.Abs(float64(item.reset-window)) > 2 {
				high, window = 0, item.reset
			}
			if item.used > high {
				at := time.Unix(item.observed, 0).In(now.Location())
				used[dayKind(at)][at.Hour()] += item.used - high
				high = item.used
			}
		}
		// Every hour on the clock since the first sample counts, used or not.
		for at := first; at.Before(now); {
			next := at.Truncate(time.Hour).Add(time.Hour)
			if next.After(now) {
				next = now
			}
			hours[dayKind(at)][at.Hour()] += next.Sub(at).Hours()
			at = next
		}
		for hour := 0; hour < 24; hour++ {
			for day := 0; day < 2; day++ {
				// A weekend that has not been recorded yet is assumed to look like a weekday.
				from := day
				if hours[from][hour] < 0.5 {
					from = 0
				}
				if hours[from][hour] >= 0.5 {
					demand.rate[day][hour] += used[from][hour] / hours[from][hour]
				}
			}
		}
	}
	if !learned {
		return nil
	}
	return demand
}

// until returns the usage expected between now and until.
func (d *soonestResetDemand) until(now, until time.Time) float64 {
	total := 0.0
	for at := now; at.Before(until); at = at.Add(time.Hour) {
		step := math.Min(1, until.Sub(at).Hours())
		total += d.rate[dayKind(at)][at.Hour()] * step
	}
	return total
}

func (s *SoonestResetSelector) demandFor(auths []*Auth, now time.Time, fable bool) *soonestResetDemand {
	if s == nil || s.history == nil {
		return nil
	}
	ids := make([]string, 0, len(auths))
	for _, auth := range auths {
		if auth != nil {
			ids = append(ids, auth.ID)
		}
	}
	sort.Strings(ids)
	key := strings.Join(ids, "\x00")
	if fable {
		key += "\x00fable"
	}
	s.demandMu.Lock()
	defer s.demandMu.Unlock()
	entry, ok := s.demands[key]
	if !ok || now.Sub(entry.at) > soonestResetDemandTTL || now.Before(entry.at) {
		if s.demands == nil || len(s.demands) > 16 {
			s.demands = make(map[string]soonestResetDemandEntry)
		}
		entry = soonestResetDemandEntry{at: now, demand: newSoonestResetDemand(s.history, auths, now, fable)}
		s.demands[key] = entry
	}
	return entry.demand
}

// markSoonestResetReserves flags the credentials that are in their weekly reserve, or for
// a Fable model in the reserve of their Fable-only weekly window. Either forecast may be
// nil, which keeps no reserve on that window.
func markSoonestResetReserves(ranks []soonestResetRank, week, fable *soonestResetDemand, now time.Time) {
	held := func(demand *soonestResetDemand, usage float64, reset time.Time) bool {
		if demand == nil || usage < soonestResetLongWindowReserve {
			return false
		}
		return demand.until(now, reset) > soonestResetReserveCover*(soonestResetLongWindowSaturation-usage)
	}
	for i := range ranks {
		rank := &ranks[i]
		if rank.saturated {
			continue
		}
		rank.reserved = (rank.known && held(week, rank.longUsage, rank.longReset)) ||
			(rank.fableKnown && held(fable, rank.fableUsage, rank.fableReset))
	}
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
		ranks[i] = rankSoonestReset(auth, i, now, model)
	}
	// The rhythm covers every credential offered, so one in cooldown still counts as demand.
	var fableDemand *soonestResetDemand
	if soonestResetUsesFableWindow(model) {
		fableDemand = s.demandFor(auths, now, true)
	}
	markSoonestResetReserves(ranks, s.demandFor(auths, now, false), fableDemand, now)
	sort.SliceStable(ranks, func(i, j int) bool {
		return soonestResetLess(ranks[i], ranks[j])
	})
	best := ranks[0]
	if entry := selectorLogEntry(ctx); entry != nil {
		reset := "unknown"
		if best.known {
			reset = best.longReset.UTC().Format(time.RFC3339)
		}
		entry.Debugf("soonest-reset: picked auth=%s provider=%s model=%s long_reset=%s saturated=%t reserved=%t candidates=%d", best.auth.ID, provider, model, reset, best.saturated, best.reserved, len(ranks))
	}
	return best.auth, nil
}

func soonestResetLess(a, b soonestResetRank) bool {
	if a.saturated != b.saturated {
		return !a.saturated
	}
	if a.reserved != b.reserved {
		return !a.reserved
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

func rankSoonestReset(auth *Auth, index int, now time.Time, model string) soonestResetRank {
	return rankSoonestResetWith(auth, index, now, soonestResetShortWindowSaturation, model)
}

// soonestResetDraining reports whether established sessions for the model should leave
// the credential.
func soonestResetDraining(auth *Auth, now time.Time, model string) bool {
	return rankSoonestResetWith(auth, 0, now, soonestResetShortWindowDrain, model).saturated
}

func rankSoonestResetWith(auth *Auth, index int, now time.Time, shortSaturation float64, model string) soonestResetRank {
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
		rank.longUsage = long.usage
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
	if soonestResetUsesFableWindow(model) {
		if fable, ok := claudeFableWindow(signals); ok && fable.reset.After(now) {
			rank.fableKnown = true
			rank.fableReset = fable.reset
			rank.fableUsage = fable.usage
			if fable.rejected || fable.usage >= soonestResetLongWindowSaturation {
				rank.saturated = true
			}
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
