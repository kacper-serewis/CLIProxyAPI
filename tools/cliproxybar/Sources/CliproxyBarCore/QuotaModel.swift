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
    /// When the pool runs dry at `burnPerHour`, spending it the soonest-reset way. nil = it lasts.
    public var emptyAt: Date?
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
    /// Weekly burn over the last 5h, for "right now" next to the 24h average.
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
        var a = accounts
        let capacity = Double(a.count) * 100
        guard capacity > 0 else { return (nil, 100) }
        var lowest = a.reduce(0) { $0 + $1.remaining } / capacity * 100
        guard burnPerHour > 0 else { return (nil, lowest) }
        var emptyAt: Date?
        var t = now
        let end = now.addingTimeInterval(horizon)
        while t < end {
            t = t.addingTimeInterval(step)
            for i in a.indices where a[i].resetAt <= t {
                a[i].remaining = 100
                while a[i].resetAt <= t { a[i].resetAt.addTimeInterval(a[i].period) }
            }
            var need = burnPerHour * step / 3600
            for i in a.indices.sorted(by: { a[$0].resetAt < a[$1].resetAt }) where need > 0 {
                let take = min(need, a[i].remaining)
                a[i].remaining -= take
                need -= take
            }
            if need > 1e-9, emptyAt == nil { emptyAt = t }
            lowest = min(lowest, a.reduce(0) { $0 + $1.remaining } / capacity * 100)
        }
        return (emptyAt, lowest)
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
    public static func pools(points: [HistoryPoint], activeAuths: Set<String>, now: Date) -> [PoolState] {
        var byAuth: [String: [HistoryPoint]] = [:]
        for p in points where activeAuths.contains(p.auth) { byAuth[p.auth, default: []].append(p) }
        var accounts: [AccountState] = []
        let newest = points.map(\.t).max() ?? 0  // proxy liveness: Codex is only sampled on traffic
        for (auth, list) in byAuth {
            let sorted = list.sorted { $0.t < $1.t }
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
                sum + burn(byAuth[a.id]!.sorted { $0.t < $1.t }, short: false, hours: 5, now: n)
            }
            pools.append(PoolState(provider: provider, accounts: accts,
                                   short: poolWindow(accts.compactMap(\.short), burnHours: shortBurnHours,
                                                     horizon: shortPeriod, watchBelow: 20, now: now),
                                   long: poolWindow(accts.compactMap(\.long), burnHours: longBurnHours,
                                                    horizon: longPeriod, watchBelow: 15, now: now),
                                   longBurnRecent: recent,
                                   dataAge: n - newest))
        }
        return pools
    }

    static func poolWindow(_ windows: [WindowState], burnHours: Double, horizon: TimeInterval,
                           watchBelow: Double, now: Date) -> PoolWindow? {
        guard !windows.isEmpty else { return nil }
        let burn = windows.reduce(0) { $0 + $1.burnPerHour }
        let sim = simulate(windows.map {
            SimAccount(remaining: 100 - $0.used, resetAt: $0.resetAt ?? now.addingTimeInterval($0.period), period: $0.period)
        }, burnPerHour: burn, now: now, horizon: horizon)
        let level: Level = sim.emptyAt != nil ? .over : (sim.lowestLeft < watchBelow ? .watch : .ok)
        return PoolWindow(used: windows.reduce(0) { $0 + $1.used } / Double(windows.count),
                          burnPerHour: burn, burnWindowHours: burnHours,
                          sustainablePerHour: Double(windows.count) * 100 / (horizon / 3600),
                          emptyAt: sim.emptyAt, lowestLeft: sim.lowestLeft, level: level)
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
        let cutoff = now.timeIntervalSince1970 - 8 * 86400
        let data = (try? Data(contentsOf: dir.appendingPathComponent("quota-history.jsonl"))) ?? Data()
        let points = QuotaModel.parse(data).filter { $0.t >= cutoff }
        return QuotaModel.pools(points: points, activeAuths: activeAuths(in: dir), now: now)
    }
}
