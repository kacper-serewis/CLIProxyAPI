import Foundation

/// One line of ~/.cli-proxy-api/quota-history.jsonl. Usage is 0...1, times are unix seconds.
public struct HistoryPoint: Decodable, Sendable, Equatable {
    public var t: Double
    public var observed: Double?
    public var auth: String
    public var provider: String
    public var long_used: Double?
    public var long_reset: Double?
    public var short_used: Double?
    public var short_reset: Double?

    public init(t: Double, auth: String, provider: String = "claude",
                long_used: Double? = nil, long_reset: Double? = nil,
                short_used: Double? = nil, short_reset: Double? = nil, observed: Double? = nil) {
        self.t = t; self.auth = auth; self.provider = provider
        self.long_used = long_used; self.long_reset = long_reset
        self.short_used = short_used; self.short_reset = short_reset; self.observed = observed
    }
}

public enum Level: Int, Comparable, Sendable {
    case ok, watch, over, stale
    public static func < (a: Level, b: Level) -> Bool { a.rawValue < b.rawValue }
}

/// One account's 5h or weekly window. Percentages are 0...100.
public struct WindowState: Sendable {
    public var used: Double
    /// nil for a 5h window that has expired: the next one starts on first use.
    public var resetAt: Date?
    public var period: TimeInterval
    /// This account's own burn, percentage points per hour.
    public var burnPerHour: Double
    /// Fraction of the window that has passed, 0...1.
    public var elapsed: Double?
    /// Usage at reset if this account keeps burning at `burnPerHour`.
    public var projectedAtReset: Double?
}

public struct AccountState: Sendable, Identifiable {
    public var id: String
    public var name: String
    public var provider: String
    public var short: WindowState?
    public var long: WindowState?
    public var observedAt: Date
    public var nextChat = false
}

public struct PoolWindow: Sendable {
    /// Average used across the pool's accounts, 0...100.
    public var used: Double
    /// Pool burn in percentage points (of one account) per hour, and the window it was measured over.
    public var burnPerHour: Double
    public var burnWindowHours: Double
    /// Burn that would spend exactly the pool's capacity per period.
    public var sustainablePerHour: Double
    /// When the pool runs dry, spending it the soonest-reset way. nil = it lasts.
    public var emptyAt: Date?
    /// The projection follows the learned usage rhythm instead of holding `burnPerHour`
    /// constant. For the weekly window `burnPerHour` is then the usual week's average.
    public var learned = false
    /// Lowest pool level over the projection, 0...100 of capacity.
    public var lowestLeft: Double
    public var level: Level
    public var pace: Double { sustainablePerHour > 0 ? burnPerHour / sustainablePerHour : 0 }
}

public struct PoolState: Sendable {
    public var provider: String
    public var accounts: [AccountState]
    public var short: PoolWindow?
    public var long: PoolWindow?
    /// Weekly burn over the last 5h, for "right now" next to the usual pace.
    public var longBurnRecent: Double
    public var dataAge: TimeInterval
    public var level: Level {
        if dataAge > QuotaModel.staleAfter { return .stale }
        return max(short?.level ?? .ok, long?.level ?? .ok)
    }
}

public enum QuotaModel {
    public static let shortPeriod: TimeInterval = 5 * 3600
    public static let longPeriod: TimeInterval = 7 * 86400
    /// The proxy samples every 2 min; older than this means it is down or asleep.
    public static let staleAfter: TimeInterval = 10 * 60
    /// Burn windows: 5h pace from the last hour, weekly pace from the last day (covers the night).
    public static let shortBurnHours = 1.0
    public static let longBurnHours = 24.0
    /// "Right now" for the weekly window: what the learned rhythm is compared with.
    public static let recentLongHours = 5.0
    /// Router thresholds for new chats (selector_soonest_reset.go).
    public static let newChatShortLimit = 90.0
    public static let newChatLongLimit = 99.0

    public static func parse(_ data: Data) -> [HistoryPoint] {
        let decoder = JSONDecoder()
        var out: [HistoryPoint] = []
        for line in data.split(separator: UInt8(ascii: "\n")) {
            if let p = try? decoder.decode(HistoryPoint.self, from: Data(line)) { out.append(p) }
        }
        return out
    }

    /// "claude-673e3645-alice_work@example.com.json" -> "alice_work"
    public static func displayName(_ auth: String) -> String {
        var s = auth
        if s.hasSuffix(".json") { s.removeLast(5) }
        let parts = s.split(separator: "-", maxSplits: 2)
        if parts.count == 3 { s = String(parts[2]) }
        if let at = s.firstIndex(of: "@") { s = String(s[..<at]) }
        return s
    }

    /// Percentage points consumed per hour over the last `hours`. Only increases inside one
    /// window count; when a window rolled over, usage in the new one counts from zero.
    public static func burn(_ points: [HistoryPoint], short: Bool, hours: Double, now: Double) -> Double {
        let start = now - hours * 3600
        var prev: (used: Double, reset: Double)?
        var consumed = 0.0
        for p in points {
            guard let cur = value(p, short: short) else { continue }
            if p.t <= start { prev = cur; continue }
            if p.t > now { break }
            if let pr = prev {
                if abs(pr.reset - cur.reset) < 600 { consumed += max(0, cur.used - pr.used) }
                else if pr.reset <= p.t { consumed += cur.used }
            }
            prev = cur
        }
        return hours > 0 ? consumed / hours : 0
    }

    private static func value(_ p: HistoryPoint, short: Bool) -> (used: Double, reset: Double)? {
        if short {
            guard let r = p.short_reset, r > 0 else { return nil }
            return ((p.short_used ?? 0) * 100, r)  // the proxy omits short_used when it is 0
        }
        guard let r = p.long_reset, r > 0 else { return nil }
        return ((p.long_used ?? 0) * 100, r)
    }

    public struct SimAccount: Sendable {
        public var remaining: Double
        public var resetAt: Date
        public var period: TimeInterval
        public init(remaining: Double, resetAt: Date, period: TimeInterval) {
            self.remaining = remaining; self.resetAt = resetAt; self.period = period
        }
    }

    /// Spend `burnPerHour` from the account that resets soonest (soonest-reset routing),
    /// refilling each at its reset. Returns when nothing is left and the lowest pool level.
    public static func simulate(_ accounts: [SimAccount], burnPerHour: Double, now: Date,
                                horizon: TimeInterval, step: TimeInterval = 300) -> (emptyAt: Date?, lowestLeft: Double) {
        let sim = simulate(accounts, rate: { _ in burnPerHour }, now: now, horizon: horizon, step: step)
        return (sim.emptyAt, sim.lowestLeft)
    }

    /// The same with a burn that varies over time (points per hour at a given moment).
    /// `usedAtReset` is each account's usage just before its first reset, nil if none came.
    public static func simulate(_ accounts: [SimAccount], rate: (Date) -> Double, now: Date,
                                horizon: TimeInterval, step: TimeInterval = 300)
        -> (emptyAt: Date?, lowestLeft: Double, usedAtReset: [Double?]) {
        var a = accounts
        var usedAtReset = [Double?](repeating: nil, count: a.count)
        let capacity = Double(a.count) * 100
        guard capacity > 0 else { return (nil, 100, usedAtReset) }
        var lowest = a.reduce(0) { $0 + $1.remaining } / capacity * 100
        var emptyAt: Date?
        var t = now
        let end = now.addingTimeInterval(horizon)
        while t < end {
            let mid = t.addingTimeInterval(step / 2)
            t = t.addingTimeInterval(step)
            for i in a.indices where a[i].resetAt <= t {
                if usedAtReset[i] == nil { usedAtReset[i] = 100 - a[i].remaining }
                a[i].remaining = 100
                while a[i].resetAt <= t { a[i].resetAt.addTimeInterval(a[i].period) }
            }
            var need = max(0, rate(mid)) * step / 3600
            for i in a.indices.sorted(by: { a[$0].resetAt < a[$1].resetAt }) where need > 0 {
                let take = min(need, a[i].remaining)
                a[i].remaining -= take
                need -= take
            }
            if need > 1e-9, emptyAt == nil { emptyAt = t }
            lowest = min(lowest, a.reduce(0) { $0 + $1.remaining } / capacity * 100)
        }
        return (emptyAt, lowest, usedAtReset)
    }

    public static func account(auth: String, points: [HistoryPoint], now: Date) -> AccountState? {
        guard let latest = points.last else { return nil }
        let n = now.timeIntervalSince1970
        var state = AccountState(id: auth, name: displayName(auth), provider: latest.provider,
                                 observedAt: Date(timeIntervalSince1970: latest.observed ?? latest.t))

        if let r = latest.long_reset, r > 0 {
            var reset = r
            let rolled = reset <= n
            while reset <= n { reset += longPeriod }
            let used = rolled ? 0 : (latest.long_used ?? 0) * 100
            let burn = burn(points, short: false, hours: longBurnHours, now: n)
            state.long = WindowState(used: used, resetAt: Date(timeIntervalSince1970: reset), period: longPeriod,
                                     burnPerHour: burn,
                                     elapsed: min(1, max(0, 1 - (reset - n) / longPeriod)),
                                     projectedAtReset: used + burn * (reset - n) / 3600)
        }
        if let r = latest.short_reset, r > 0 {
            let burn = burn(points, short: true, hours: shortBurnHours, now: n)
            if r > n {
                let used = (latest.short_used ?? 0) * 100
                state.short = WindowState(used: used, resetAt: Date(timeIntervalSince1970: r), period: shortPeriod,
                                          burnPerHour: burn,
                                          elapsed: min(1, max(0, 1 - (r - n) / shortPeriod)),
                                          projectedAtReset: used + burn * (r - n) / 3600)
            } else {
                state.short = WindowState(used: 0, resetAt: nil, period: shortPeriod, burnPerHour: burn,
                                          elapsed: nil, projectedAtReset: nil)
            }
        }
        return state
    }

    /// One pool per provider, from the history of the active (not disabled) auth files.
    /// `points` may hold up to the proxy's 90 days and accounts that are no longer active: the
    /// usage rhythm is learned from all of them.
    public static func pools(points: [HistoryPoint], activeAuths: Set<String>, now: Date,
                             calendar: Calendar = .current) -> [PoolState] {
        var all: [String: [HistoryPoint]] = [:]
        for p in points { all[p.auth, default: []].append(p) }
        all = all.mapValues { $0.sorted { $0.t < $1.t } }
        let byAuth = all.filter { activeAuths.contains($0.key) }
        var accounts: [AccountState] = []
        let newest = points.map(\.t).max() ?? 0  // proxy liveness: Codex is only sampled on traffic
        for (auth, sorted) in byAuth {
            if let a = account(auth: auth, points: sorted, now: now) {
                accounts.append(a)
            }
        }
        let n = now.timeIntervalSince1970
        var pools: [PoolState] = []
        for provider in Set(accounts.map(\.provider)).sorted() {
            var accts = accounts.filter { $0.provider == provider }.sorted { $0.name < $1.name }
            markNextChat(&accts)
            let recent = accts.reduce(0.0) { sum, a in
                sum + burn(byAuth[a.id]!, short: false, hours: recentLongHours, now: n)
            }
            let profile = learnProfile(shape: all.values.filter { $0.first?.provider == provider },
                                       pool: accts.map { byAuth[$0.id]! }, now: now, calendar: calendar)
            let rate = profile.map {
                projectedRate($0, recentBurn: recent, recentHours: recentLongHours, now: now, calendar: calendar)
            }
            let withLong = accts.indices.filter { accts[$0].long != nil }
            let long = poolWindow(withLong.map { accts[$0].long! }, burnHours: longBurnHours,
                                  horizon: longPeriod, watchBelow: 15, now: now,
                                  rate: rate, typicalPerHour: profile.map { $0.perWeek / (longPeriod / 3600) })
            // With a learned rhythm, each account's weekly outlook is its share of the pool
            // projection instead of its own 24h burn held constant.
            if let long, long.window.learned {
                for (k, i) in withLong.enumerated() { accts[i].long!.projectedAtReset = long.usedAtReset[k] }
            }
            // The 5h window follows the same rhythm, converted to its own points, but leans on
            // the last hour's burn. burnPerHour stays that last hour.
            let shorts = accts.compactMap(\.short)
            var shortRate: (@Sendable (Date) -> Double)?
            if let profile, let scale = shortPerLongPoint(accts.map { byAuth[$0.id]! }, now: now) {
                let poolBurn = shorts.reduce(0) { $0 + $1.burnPerHour }
                shortRate = reactiveRate(profile, scale: scale, recentBurn: poolBurn,
                                         recentHours: shortBurnHours, now: now, calendar: calendar)
                // Each account's 5h outlook: its own current burn, then its share of the rhythm.
                let shares = rhythmShares(accts)
                for i in accts.indices {
                    guard let w = accts[i].short, let reset = w.resetAt else { continue }
                    let rate = reactiveRate(profile, scale: scale * shares[i], recentBurn: w.burnPerHour,
                                            recentHours: shortBurnHours, now: now, calendar: calendar)
                    accts[i].short!.projectedAtReset = w.used + spent(rate, from: now, to: reset)
                }
            }
            pools.append(PoolState(provider: provider, accounts: accts,
                                   short: poolWindow(shorts, burnHours: shortBurnHours,
                                                     horizon: shortPeriod, watchBelow: 20, now: now,
                                                     rate: shortRate)?.window,
                                   long: long?.window,
                                   longBurnRecent: recent,
                                   dataAge: n - newest))
        }
        return pools
    }

    static func poolWindow(_ windows: [WindowState], burnHours: Double, horizon: TimeInterval,
                           watchBelow: Double, now: Date, rate: ((Date) -> Double)? = nil,
                           typicalPerHour: Double? = nil) -> (window: PoolWindow, usedAtReset: [Double?])? {
        guard !windows.isEmpty else { return nil }
        let burn = windows.reduce(0) { $0 + $1.burnPerHour }
        let sim = simulate(windows.map {
            SimAccount(remaining: 100 - $0.used, resetAt: $0.resetAt ?? now.addingTimeInterval($0.period), period: $0.period)
        }, rate: rate ?? { _ in burn }, now: now, horizon: horizon)
        let level: Level = sim.emptyAt != nil ? .over : (sim.lowestLeft < watchBelow ? .watch : .ok)
        let learned = rate != nil
        return (PoolWindow(used: windows.reduce(0) { $0 + $1.used } / Double(windows.count),
                           burnPerHour: typicalPerHour ?? burn, burnWindowHours: burnHours,
                           sustainablePerHour: Double(windows.count) * 100 / (horizon / 3600),
                           emptyAt: sim.emptyAt, learned: learned, lowestLeft: sim.lowestLeft, level: level),
                sim.usedAtReset)
    }

    /// Soonest-reset: new chats go to the account whose weekly window resets first, unless it is
    /// past the new-chat thresholds.
    static func markNextChat(_ accounts: inout [AccountState]) {
        let candidates = accounts.indices.filter {
            (accounts[$0].short?.used ?? 0) < newChatShortLimit && (accounts[$0].long?.used ?? 0) < newChatLongLimit
        }
        if let i = candidates.min(by: {
            (accounts[$0].long?.resetAt ?? .distantFuture) < (accounts[$1].long?.resetAt ?? .distantFuture)
        }) { accounts[i].nextChat = true }
    }
}

/// The usage rhythm learned from history: expected pool burn in percentage points per hour for
/// each local hour of a weekday and of a weekend day.
public struct UsageProfile: Sendable {
    public var weekday: [Double]
    public var weekend: [Double]
    /// Points a typical week burns.
    public var perWeek: Double { 5 * weekday.reduce(0, +) + 2 * weekend.reduce(0, +) }

    public func rate(at date: Date, calendar: Calendar = .current) -> Double {
        let slot = UsageProfile.slot(date.timeIntervalSince1970, calendar)
        return slot < 24 ? weekday[slot] : weekend[slot - 24]
    }

    /// Slot 0..<24 is a weekday hour, 24..<48 a weekend hour, in the calendar's time zone.
    static func slot(_ t: Double, _ calendar: Calendar) -> Int {
        let c = calendar.dateComponents([.hour, .weekday], from: Date(timeIntervalSince1970: t))
        let weekend = c.weekday == 1 || c.weekday == 7
        return (weekend ? 24 : 0) + (c.hour ?? 0)
    }
}

extension QuotaModel {
    /// Below this much history the rhythm is noise; the projection stays at the recent pace.
    static let minProfileHours = 48.0
    /// The level follows how the pool has been used lately, not how it was used a month ago.
    static let levelWindow: TimeInterval = 14 * 86400
    // How the recent burn is blended into the rhythm. The values come from the panel's
    // backtest on the recorded history (scripts/poolBacktest.ts, RATE_TUNING in
    // quotaPoolModel.ts) and must stay the same as there: the rhythm carries the projection,
    // the recent pace only nudges it.
    /// How fast "busier (or quieter) than usual right now" fades back to the learned rhythm.
    static let recentHalfLife: TimeInterval = 1.5 * 3600
    /// The same for the 5h window, where a busy day stays busy for longer.
    static let shortRecentHalfLife: TimeInterval = 6 * 3600
    /// The recent pace counts for at most this many times the usual pace.
    static let maxRecentFactor = 2.0
    /// How long burn at a normally idle hour is assumed to carry on.
    static let sessionHalfLife: TimeInterval = 1800
    /// 5h window: how long the current burn itself is assumed to carry on before the rhythm
    /// takes over.
    static let currentHalfLife: TimeInterval = 3600

    /// Points one account used, as (time, points) at the sample that revealed them.
    /// Readings jitter by a point, so only a new high within a reset window counts. A 5h
    /// window that follows an expired one counts from zero; the weekly one starts at its
    /// first reading, like the panel's model.
    static func usageEvents(_ points: [HistoryPoint], short: Bool = false) -> [(t: Double, amount: Double)] {
        var events: [(Double, Double)] = []
        var reset: Double?
        var high = 0.0
        for p in points {
            guard let cur = value(p, short: short) else { continue }
            if let r = reset, abs(r - cur.reset) < 600 {
                if cur.used > high { events.append((p.t, cur.used - high)); high = cur.used }
            } else {
                if short, let r = reset, r <= p.t, cur.used > 0 { events.append((p.t, cur.used)) }
                reset = cur.reset; high = cur.used
            }
        }
        return events
    }

    /// How many 5h-window points one weekly point costs, from what the pool's accounts burned
    /// in both windows over the last two weeks. It turns the weekly rhythm into 5h points.
    static func shortPerLongPoint(_ pool: [[HistoryPoint]], now: Date) -> Double? {
        let n = now.timeIntervalSince1970
        func total(short: Bool) -> Double {
            pool.flatMap { usageEvents($0, short: short) }
                .reduce(0.0) { $1.t > n - levelWindow && $1.t <= n ? $0 + $1.amount : $0 }
        }
        let long = total(short: false), short = total(short: true)
        // A handful of weekly points is too coarse to divide by.
        return long >= 5 && short > 0 ? short / long : nil
    }

    /// How the pool's rhythm is split over its accounts for the 5h outlook: chats stay on their
    /// account, so in proportion to what each is burning now. With nothing burning, the next
    /// chat's account takes it all.
    static func rhythmShares(_ accounts: [AccountState]) -> [Double] {
        let burns = accounts.map { $0.short?.burnPerHour ?? 0 }
        let total = burns.reduce(0, +)
        if total > 0 { return burns.map { $0 / total } }
        if accounts.contains(where: \.nextChat) { return accounts.map { $0.nextChat ? 1 : 0 } }
        return accounts.map { _ in 1 / Double(max(1, accounts.count)) }
    }

    /// Points burned between two instants at a varying rate (points per hour).
    static func spent(_ rate: (Date) -> Double, from: Date, to: Date, step: TimeInterval = 300) -> Double {
        var total = 0.0
        var t = from
        while t < to {
            let next = min(t.addingTimeInterval(step), to)
            total += max(0, rate(t.addingTimeInterval(next.timeIntervalSince(t) / 2))) * next.timeIntervalSince(t) / 3600
            t = next
        }
        return total
    }

    /// Burn for the 5h window, in its own points per hour. It starts at the current burn and
    /// keeps a share of it that halves every `currentHalfLife`; the rest follows the rhythm
    /// (`scale` × profile), scaled by how today compares with the usual and fading like the
    /// weekly one. So it reacts to what is happening now, yet still winds down towards the
    /// night and picks up before a working morning.
    public static func reactiveRate(_ profile: UsageProfile, scale: Double, recentBurn: Double,
                                    recentHours: Double, now: Date,
                                    calendar: Calendar = .current) -> @Sendable (Date) -> Double {
        let samples = max(1, Int((recentHours * 4).rounded()))
        var usual = 0.0
        for i in 0..<samples {
            let back = (Double(i) + 0.5) / Double(samples) * recentHours * 3600
            usual += scale * profile.rate(at: now.addingTimeInterval(-back), calendar: calendar)
        }
        usual /= Double(samples)
        let factor = usual > 0 ? min(maxRecentFactor, recentBurn / usual) : 1
        return { t in
            let elapsed = max(0, t.timeIntervalSince(now))
            let current = pow(0.5, elapsed / currentHalfLife)
            let rhythm = scale * profile.rate(at: t, calendar: calendar)
                * (1 + (factor - 1) * pow(0.5, elapsed / shortRecentHalfLife))
            return current * recentBurn + (1 - current) * rhythm
        }
    }

    /// Hours each slot was covered between two instants (partial hours count partially).
    static func slotExposure(from: Double, to: Double, calendar: Calendar) -> [Double] {
        var hours = [Double](repeating: 0, count: 48)
        var t = from
        while t < to {
            let end = min((t / 3600).rounded(.down) * 3600 + 3600, to)
            hours[UsageProfile.slot(t, calendar)] += (end - t) / 3600
            t = end
        }
        return hours
    }

    /// Learn when the pool gets used. The shape (which hours and day types are busy) comes from
    /// every account in `shape`, including ones no longer in the pool, because a pool that
    /// changed last week has too little history of its own. The level comes from `pool`, the
    /// current accounts, over the time they all have history. Each list is sorted by time.
    ///
    /// Time with no samples (the proxy was off, the Mac asleep) counts as observed and idle:
    /// usage made elsewhere shows up on the next sample.
    public static func learnProfile(shape: [[HistoryPoint]], pool: [[HistoryPoint]], now: Date,
                                    calendar: Calendar = .current) -> UsageProfile? {
        let n = now.timeIntervalSince1970
        let lists = shape.filter { !$0.isEmpty }
        guard let first = lists.map({ $0[0].t }).min(), (n - first) / 3600 >= minProfileHours else { return nil }

        let exposure = slotExposure(from: first, to: n, calendar: calendar)
        var used = [Double](repeating: 0, count: 48)
        for list in lists {
            for e in usageEvents(list) where e.t <= n { used[UsageProfile.slot(e.t, calendar)] += e.amount }
        }
        // Smooth over neighbouring hours; a few weeks give each hour only a handful of samples.
        func shapeOf(_ offset: Int) -> [Double] {
            (0..<24).map { hour in
                var points = 0.0, hours = 0.0
                for (shift, weight) in [(-1, 1.0), (0, 2.0), (1, 1.0)] {
                    let slot = offset + (hour + shift + 24) % 24
                    points += weight * used[slot]
                    hours += weight * exposure[slot]
                }
                return hours > 0 ? points / hours : 0
            }
        }
        var profile = UsageProfile(weekday: shapeOf(0), weekend: [])
        // No weekend seen yet: assume it looks like a weekday until one has been recorded.
        profile.weekend = exposure[24...].reduce(0, +) >= 24 ? shapeOf(24) : profile.weekday

        // Scale the shape so it reproduces what the current accounts actually burned.
        let tracked = pool.filter { !$0.isEmpty }
        if !tracked.isEmpty {
            let from = max(n - levelWindow, tracked.map { $0[0].t }.max()!)
            if n - from >= 86400 {
                let slots = slotExposure(from: from, to: n, calendar: calendar)
                let expected = (0..<48).reduce(0.0) {
                    $0 + slots[$1] * ($1 < 24 ? profile.weekday[$1] : profile.weekend[$1 - 24])
                }
                let actual = tracked.flatMap { usageEvents($0) }
                    .reduce(0.0) { $1.t > from && $1.t <= n ? $0 + $1.amount : $0 }
                if expected > 0 {
                    let scale = actual / expected
                    profile.weekday = profile.weekday.map { $0 * scale }
                    profile.weekend = profile.weekend.map { $0 * scale }
                }
            }
        }
        return profile.perWeek > 0 ? profile : nil
    }

    /// Burn to project with: the learned rhythm, scaled by how much busier or quieter than usual
    /// the last `recentHours` were. That factor fades back to 1 over a few hours, and it
    /// multiplies the rhythm, so a busy evening does not turn into a busy night. Burn the rhythm
    /// cannot explain at all (working at an hour that is normally idle) carries on briefly.
    public static func projectedRate(_ profile: UsageProfile, recentBurn: Double, recentHours: Double,
                                     now: Date, calendar: Calendar = .current) -> @Sendable (Date) -> Double {
        let samples = max(1, Int((recentHours * 4).rounded()))
        var usual = 0.0
        for i in 0..<samples {
            let back = (Double(i) + 0.5) / Double(samples) * recentHours * 3600
            usual += profile.rate(at: now.addingTimeInterval(-back), calendar: calendar)
        }
        usual /= Double(samples)
        let factor = usual > 0 ? min(maxRecentFactor, recentBurn / usual) : 1
        let unexplained = max(0, recentBurn - factor * usual)
        return { t in
            let elapsed = max(0, t.timeIntervalSince(now))
            return profile.rate(at: t, calendar: calendar) * (1 + (factor - 1) * pow(0.5, elapsed / recentHalfLife))
                + unexplained * pow(0.5, elapsed / sessionHalfLife)
        }
    }
}

extension QuotaModel {
    /// Colour of one account's window row. An account that fills up before its reset is only a
    /// problem when the pool is: otherwise the router moves chats to another account. So the
    /// row is never more alarming than its pool, unless the data is stale.
    public static func accountLevel(_ window: WindowState, pool: Level) -> Level {
        let projected = window.projectedAtReset ?? 0
        let own: Level = projected >= 100 ? .over : (window.used >= 90 || projected >= 90 ? .watch : .ok)
        return pool == .stale ? own : min(own, pool)
    }
}

/// A pool that is projected to run dry, worth a notification.
public struct PoolAlert: Equatable, Sendable {
    /// "<provider>.short" or "<provider>.long".
    public var key: String
    public var provider: String
    public var short: Bool
    public var emptyAt: Date

    public init(key: String, provider: String, short: Bool, emptyAt: Date) {
        self.key = key; self.provider = provider; self.short = short; self.emptyAt = emptyAt
    }
}

/// Decides when to notify: once a pool has been projected to run out for a couple of minutes,
/// and then not again for that window until its cooldown has passed.
public struct PoolAlerter: Sendable {
    /// A projection that flips for one refresh is not worth a notification.
    public static let holdFor: TimeInterval = 120
    /// One alert per window: 5h for the 5-hour pool, a day for the weekly one.
    public static func cooldown(short: Bool) -> TimeInterval { short ? 5 * 3600 : 24 * 3600 }

    /// When each key was last alerted; persist it so a restart does not alert again.
    public private(set) var lastSent: [String: Date]
    private var overSince: [String: Date] = [:]

    public init(lastSent: [String: Date] = [:]) { self.lastSent = lastSent }

    public mutating func check(_ pools: [PoolState], now: Date) -> [PoolAlert] {
        var alerts: [PoolAlert] = []
        var over: Set<String> = []
        for pool in pools where pool.level != .stale {
            for (short, window) in [(true, pool.short), (false, pool.long)] {
                guard let emptyAt = window?.emptyAt else { continue }
                let key = "\(pool.provider).\(short ? "short" : "long")"
                over.insert(key)
                let since = overSince[key] ?? now
                overSince[key] = since
                guard now.timeIntervalSince(since) >= Self.holdFor else { continue }
                if let last = lastSent[key], now.timeIntervalSince(last) < Self.cooldown(short: short) { continue }
                lastSent[key] = now
                alerts.append(PoolAlert(key: key, provider: pool.provider, short: short, emptyAt: emptyAt))
            }
        }
        overSince = overSince.filter { over.contains($0.key) }
        return alerts
    }
}

/// Reads the proxy's files. Only the `disabled` field of each auth file is decoded.
public enum ProxyFiles {
    public static let dir = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent(".cli-proxy-api")

    public static func activeAuths(in dir: URL = dir) -> Set<String> {
        struct Flag: Decodable { var disabled: Bool? }
        let names = (try? FileManager.default.contentsOfDirectory(atPath: dir.path)) ?? []
        return Set(names.filter { name in
            guard name.hasSuffix(".json"), name.hasPrefix("claude-") || name.hasPrefix("codex-"),
                  let data = try? Data(contentsOf: dir.appendingPathComponent(name)),
                  let flag = try? JSONDecoder().decode(Flag.self, from: data) else { return false }
            return flag.disabled != true
        })
    }

    public static func load(now: Date = Date(), dir: URL = dir) -> [PoolState] {
        // Everything the proxy keeps (90 days): the usage rhythm is learned from it.
        let cutoff = now.timeIntervalSince1970 - 91 * 86400
        let data = (try? Data(contentsOf: dir.appendingPathComponent("quota-history.jsonl"))) ?? Data()
        let points = QuotaModel.parse(data).filter { $0.t >= cutoff }
        return QuotaModel.pools(points: points, activeAuths: activeAuths(in: dir), now: now)
    }
}
