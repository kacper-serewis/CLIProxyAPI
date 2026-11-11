import Foundation
import Testing
@testable import CliproxyBarCore

@Test func displayNameStripsProviderHashAndDomain() {
    #expect(QuotaModel.displayName("claude-673e3645-alice_work@example.com.json") == "alice_work")
    #expect(QuotaModel.displayName("codex-07a82941-team@example.com-prolite.json") == "team")
}

@Test func burnCountsIncreasesAndNewWindowFromZero() {
    let pts = [
        HistoryPoint(t: 0, auth: "a", short_used: 0.10, short_reset: 1000),
        HistoryPoint(t: 600, auth: "a", short_used: 0.20, short_reset: 1000),
        // window rolled over at 1000; new window already at 5%
        HistoryPoint(t: 1200, auth: "a", short_used: 0.05, short_reset: 19000),
    ]
    let b = QuotaModel.burn(pts, short: true, hours: 1, now: 3600)
    #expect(abs(b - 15) < 1e-9)
}

@Test func missingShortUsedMeansZero() {
    let now = Date(timeIntervalSince1970: 10_000)
    let a = QuotaModel.account(auth: "a", points: [
        HistoryPoint(t: 9_900, auth: "a", long_used: 0, long_reset: 20_000, short_reset: 12_000),
    ], now: now)
    #expect(a?.short?.used == 0)
    #expect(a?.short?.resetAt == Date(timeIntervalSince1970: 12_000))
}

@Test func simulationDrainsSoonestResetAndRefills() {
    let now = Date(timeIntervalSince1970: 0)
    // Two accounts at 50% left; 20 pts/h for 5h needs 100 pts — exactly the pool.
    let accts = [
        QuotaModel.SimAccount(remaining: 50, resetAt: now.addingTimeInterval(10 * 3600), period: 5 * 3600),
        QuotaModel.SimAccount(remaining: 50, resetAt: now.addingTimeInterval(20 * 3600), period: 5 * 3600),
    ]
    let lasts = QuotaModel.simulate(accts, burnPerHour: 19, now: now, horizon: 5 * 3600)
    #expect(lasts.emptyAt == nil)
    let runsOut = QuotaModel.simulate(accts, burnPerHour: 30, now: now, horizon: 5 * 3600)
    #expect(runsOut.emptyAt != nil)
    // A reset half-way refills the first account, so 30 pts/h survives 5h.
    var early = accts
    early[0].resetAt = now.addingTimeInterval(2 * 3600)
    #expect(QuotaModel.simulate(early, burnPerHour: 30, now: now, horizon: 5 * 3600).emptyAt == nil)
}

@Test func nextChatIsSoonestResetUnderThresholds() {
    let now = Date(timeIntervalSince1970: 0)
    func acct(_ id: String, short: Double, longReset: Double) -> AccountState {
        AccountState(id: id, name: id, provider: "claude",
                     short: WindowState(used: short, resetAt: nil, period: 1, burnPerHour: 0),
                     long: WindowState(used: 10, resetAt: now.addingTimeInterval(longReset), period: 1, burnPerHour: 0),
                     observedAt: now)
    }
    var accounts = [acct("a", short: 95, longReset: 100), acct("b", short: 10, longReset: 200)]
    QuotaModel.markNextChat(&accounts)
    #expect(accounts.map(\.nextChat) == [false, true])
}

// MARK: - Usage rhythm

private let utc: Calendar = {
    var c = Calendar(identifier: .gregorian)
    c.timeZone = TimeZone(identifier: "UTC")!
    return c
}()

/// 2026-11-<day> <hour>:<minute> UTC. The 11th is a Wednesday.
private func at(_ day: Int, _ hour: Int, _ minute: Int = 0) -> Double {
    utc.date(from: DateComponents(year: 2026, month: 11, day: day, hour: hour, minute: minute))!.timeIntervalSince1970
}

/// One point per hour from 9:00 to 17:00 on weekdays, 10:00 to 12:00 on weekends.
private func rhythm(_ auth: String, from: Int, to: Int, perHour: Double = 1, reset: Double) -> [HistoryPoint] {
    var used = 0.0
    var points = [HistoryPoint(t: at(from, 0), auth: auth, long_used: 0, long_reset: reset)]
    for day in from...to {
        let weekday = utc.component(.weekday, from: Date(timeIntervalSince1970: at(day, 12)))
        let weekend = weekday == 1 || weekday == 7
        for hour in (weekend ? 10..<12 : 9..<17) {
            used += perHour
            points.append(HistoryPoint(t: at(day, hour, 30), auth: auth, long_used: used / 100, long_reset: reset))
        }
    }
    return points
}

@Test func profileLearnsQuietNightsAndLighterWeekends() throws {
    let now = Date(timeIntervalSince1970: at(11, 22))
    let points = rhythm("a", from: 2, to: 11, reset: at(30, 0))
    let profile = try #require(QuotaModel.learnProfile(shape: [points], pool: [points], now: now, calendar: utc))
    #expect(profile.weekday[3] == 0)
    #expect(abs(profile.weekday[13] - 1) < 0.05)
    #expect(profile.weekend[3] == 0)
    #expect(profile.weekend[11] < profile.weekday[11])
    #expect(abs(profile.perWeek - 44) < 0.5)
    // Jittering readings do not count as usage.
    let noisy = points.flatMap { p in
        [p, HistoryPoint(t: p.t + 120, auth: "a", long_used: p.long_used! - 0.01, long_reset: p.long_reset),
         HistoryPoint(t: p.t + 240, auth: "a", long_used: p.long_used, long_reset: p.long_reset)]
    }
    let again = try #require(QuotaModel.learnProfile(shape: [noisy], pool: [noisy], now: now, calendar: utc))
    #expect(abs(again.perWeek - profile.perWeek) < 1e-6)
    // Too little history: no profile.
    let short = rhythm("a", from: 11, to: 11, reset: at(30, 0))
    #expect(QuotaModel.learnProfile(shape: [short], pool: [short], now: now, calendar: utc) == nil)
}

@Test func profileTakesShapeFromEveryAccountAndLevelFromThePool() throws {
    let now = Date(timeIntervalSince1970: at(11, 22))
    let old = rhythm("old", from: 1, to: 7, reset: at(30, 0))
    let mine = rhythm("a", from: 8, to: 11, perHour: 2, reset: at(30, 0))
    let profile = try #require(QuotaModel.learnProfile(shape: [old, mine], pool: [mine], now: now, calendar: utc))
    #expect(profile.weekday[3] == 0)
    #expect(abs(profile.weekday[13] - 2) < 0.2)
}

@Test func projectionIsFlatOvernightAndRecentPaceFades() throws {
    let now = Date(timeIntervalSince1970: at(11, 22))
    let points = rhythm("a", from: 2, to: 11, reset: at(30, 0))
    let profile = try #require(QuotaModel.learnProfile(shape: [points], pool: [points], now: now, calendar: utc))
    func date(_ day: Int, _ hour: Int) -> Date { Date(timeIntervalSince1970: at(day, hour)) }

    // 4 pts/h at 22:00, when nothing is expected: a short session, not a busy night.
    let late = QuotaModel.projectedRate(profile, recentBurn: 4, recentHours: 1, now: now, calendar: utc)
    #expect(abs(late(now) - 4) < 1e-9)
    #expect(abs(late(now.addingTimeInterval(1800)) - 2) < 1e-9)
    #expect(late(date(12, 3)) < 0.01)
    #expect(abs(late(date(13, 13)) - 1) < 0.05)
    // Twice the usual pace at 13:00 scales the rhythm and fades.
    let busy = QuotaModel.projectedRate(profile, recentBurn: 2, recentHours: 1, now: date(11, 13), calendar: utc)
    #expect(abs(busy(Date(timeIntervalSince1970: at(11, 14, 30))) - 1.5 * profile.weekday[14]) < 1e-9)
    // Four times the usual counts as twice; the rest is a session that is gone in three hours.
    let wild = QuotaModel.projectedRate(profile, recentBurn: 4, recentHours: 1, now: date(11, 13), calendar: utc)
    #expect(abs(wild(date(11, 16)) - (1.25 * profile.weekday[16] + 2.0 / 64)) < 1e-9)
    #expect(busy(date(12, 3)) < 1e-9)

    // 10 points left, reset far away: a constant 1 pt/h would empty it at 08:00, the rhythm
    // keeps it through the night and into the next working day.
    let acct = [QuotaModel.SimAccount(remaining: 10, resetAt: date(30, 0), period: 7 * 86400)]
    let quiet = QuotaModel.projectedRate(profile, recentBurn: 0, recentHours: 1, now: now, calendar: utc)
    let sim = QuotaModel.simulate(acct, rate: quiet, now: now, horizon: 7 * 86400)
    let empty = try #require(sim.emptyAt)
    #expect(empty > date(13, 9) && empty < date(13, 12))
}

@Test func poolsUseTheRhythmForTheWeeklyWindow() throws {
    let now = Date(timeIntervalSince1970: at(11, 22))
    let reset = at(13, 11)
    let points = rhythm("claude-1-a@x.json", from: 2, to: 11, reset: reset)
        + rhythm("claude-1-gone@x.json", from: 1, to: 7, reset: reset)
    let pool = try #require(QuotaModel.pools(points: points, activeAuths: ["claude-1-a@x.json"], now: now, calendar: utc).first)
    let long = try #require(pool.long)
    #expect(long.learned)
    #expect(pool.accounts.count == 1)
    // 68 used now; Thursday adds 8 and Friday morning about two more before the 11:00 reset.
    let projected = try #require(pool.accounts[0].long?.projectedAtReset)
    #expect(abs(projected - 78) < 1.5)
}

@Test func fiveHourRateLeansOnCurrentBurnThenFollowsTheRhythm() throws {
    let learnedAt = Date(timeIntervalSince1970: at(11, 22))
    let points = rhythm("a", from: 2, to: 11, reset: at(30, 0))
    let profile = try #require(QuotaModel.learnProfile(shape: [points], pool: [points], now: learnedAt, calendar: utc))
    func date(_ day: Int, _ hour: Int, _ minute: Int = 0) -> Date { Date(timeIntervalSince1970: at(day, hour, minute)) }
    let scale = 8.0  // one weekly point costs eight 5h points

    // Mid-afternoon at twice the usual: starts at the current burn; an hour on, half of it is
    // the current burn and half the (busier than usual) rhythm.
    let busy = QuotaModel.reactiveRate(profile, scale: scale, recentBurn: 16, recentHours: 1, now: date(11, 13), calendar: utc)
    #expect(abs(busy(date(11, 13)) - 16) < 1e-9)
    #expect(busy(date(11, 14)) > 8 + 0.5 * 8 * profile.weekday[14])
    // A late session: half is still there after an hour, and it winds down into the night.
    let late = QuotaModel.reactiveRate(profile, scale: scale, recentBurn: 16, recentHours: 1, now: date(11, 22), calendar: utc)
    #expect(abs(late(date(11, 23)) - 8) < 1e-9)
    #expect(late(date(12, 3)) < 2)
    // Idle before work: the working morning still shows up.
    let idle = QuotaModel.reactiveRate(profile, scale: scale, recentBurn: 0, recentHours: 1, now: date(12, 6), calendar: utc)
    #expect(idle(date(12, 6)) == 0)
    #expect(idle(date(12, 10)) > 4)
}

@Test func shortPerLongPointComparesBothWindows() throws {
    // One weekly point per hour on a working day; the 5h window climbs 8 points per sample,
    // across two 5h windows.
    var points: [HistoryPoint] = []
    for (i, hour) in (9..<17).enumerated() {
        let window = hour < 14 ? at(11, 14) : at(11, 19)
        let inWindow = Double(hour < 14 ? hour - 9 : hour - 14) + 1
        points.append(HistoryPoint(t: at(11, hour, 30), auth: "a", long_used: Double(i + 1) / 100,
                                   long_reset: at(30, 0), short_used: inWindow * 0.08, short_reset: window))
    }
    let now = Date(timeIntervalSince1970: at(11, 22))
    // Weekly: 7 points after the first reading. 5h: 4 × 8 in the first window after its first
    // reading, 3 × 8 in the second.
    let k = try #require(QuotaModel.shortPerLongPoint([points], now: now))
    #expect(abs(k - 56.0 / 7.0) < 1e-9)
    #expect(QuotaModel.shortPerLongPoint([Array(points.prefix(3))], now: now) == nil)
}

@Test func rhythmSharesFollowCurrentBurnOrTheNextChat() {
    let now = Date(timeIntervalSince1970: 0)
    func acct(_ id: String, burn: Double, next: Bool = false) -> AccountState {
        AccountState(id: id, name: id, provider: "claude",
                     short: WindowState(used: 10, resetAt: now.addingTimeInterval(3600), period: 1, burnPerHour: burn),
                     observedAt: now, nextChat: next)
    }
    #expect(QuotaModel.rhythmShares([acct("a", burn: 6), acct("b", burn: 2)]) == [0.75, 0.25])
    #expect(QuotaModel.rhythmShares([acct("a", burn: 0), acct("b", burn: 0, next: true)]) == [0, 1])
    #expect(QuotaModel.rhythmShares([acct("a", burn: 0), acct("b", burn: 0)]) == [0.5, 0.5])
    #expect(abs(QuotaModel.spent({ _ in 6 }, from: now, to: now.addingTimeInterval(5400)) - 9) < 1e-9)
}

@Test func accountFiveHourOutlookWindsDownIntoTheNight() throws {
    // A working rhythm in both windows, then a late session: 10 5h-points in the last hour at
    // 22:00, window resets at 02:00. Held constant that would be +40; the rhythm expects
    // nothing at night, so only the fading session is left.
    let auth = "claude-1-a@x.json"
    var points: [HistoryPoint] = []
    var long = 0.0
    for day in 2...11 {
        let weekday = utc.component(.weekday, from: Date(timeIntervalSince1970: at(day, 12)))
        if weekday == 1 || weekday == 7 { continue }
        for hour in 9..<17 {
            long += 1
            let window = hour < 14 ? at(day, 14) : at(day, 19)
            points.append(HistoryPoint(t: at(day, hour, 30), auth: auth, long_used: long / 100, long_reset: at(30, 0),
                                       short_used: Double(hour < 14 ? hour - 8 : hour - 13) * 0.06, short_reset: window))
        }
    }
    points.append(HistoryPoint(t: at(11, 20, 55), auth: auth, long_used: long / 100, long_reset: at(30, 0),
                               short_used: 0.02, short_reset: at(12, 2)))
    points.append(HistoryPoint(t: at(11, 21, 55), auth: auth, long_used: (long + 2) / 100, long_reset: at(30, 0),
                               short_used: 0.12, short_reset: at(12, 2)))
    let now = Date(timeIntervalSince1970: at(11, 22))
    let pool = try #require(QuotaModel.pools(points: points, activeAuths: [auth], now: now, calendar: utc).first)
    let short = try #require(pool.accounts[0].short)
    #expect(abs(short.burnPerHour - 10) < 1e-9)
    let projected = try #require(short.projectedAtReset)
    // 12 used + 10 pts/h halving every hour over 4h ≈ 12 + 13.5
    #expect(projected > 24 && projected < 28)
}

@Test func accountRowIsOnlyAsAlarmingAsItsPool() {
    let full = WindowState(used: 60, resetAt: nil, period: 1, burnPerHour: 1, projectedAtReset: 100)
    // Filling up before the reset is fine while the pool lasts: the router moves on.
    #expect(QuotaModel.accountLevel(full, pool: .ok) == .ok)
    #expect(QuotaModel.accountLevel(full, pool: .watch) == .watch)
    #expect(QuotaModel.accountLevel(full, pool: .over) == .over)
    let calm = WindowState(used: 20, resetAt: nil, period: 1, burnPerHour: 1, projectedAtReset: 40)
    #expect(QuotaModel.accountLevel(calm, pool: .over) == .ok)
    // Without fresh data the account's own outlook is all there is.
    #expect(QuotaModel.accountLevel(full, pool: .stale) == .over)
}

@Test func alertsOnceWhenAPoolKeepsRunningOut() {
    let start = Date(timeIntervalSince1970: 1_000_000)
    func pool(shortEmpty: Date?, age: TimeInterval = 0) -> PoolState {
        PoolState(provider: "claude", accounts: [],
                  short: PoolWindow(used: 80, burnPerHour: 30, burnWindowHours: 1, sustainablePerHour: 20,
                                    emptyAt: shortEmpty, lowestLeft: 0, level: shortEmpty == nil ? .ok : .over),
                  long: nil, longBurnRecent: 0, dataAge: age)
    }
    let empty = start.addingTimeInterval(3600)
    var alerter = PoolAlerter()
    // A blip is not an alert: it has to hold for a couple of minutes.
    #expect(alerter.check([pool(shortEmpty: empty)], now: start).isEmpty)
    #expect(alerter.check([pool(shortEmpty: nil)], now: start.addingTimeInterval(30)).isEmpty)
    #expect(alerter.check([pool(shortEmpty: empty)], now: start.addingTimeInterval(60)).isEmpty)
    let alerts = alerter.check([pool(shortEmpty: empty)], now: start.addingTimeInterval(200))
    #expect(alerts == [PoolAlert(key: "claude.short", provider: "claude", short: true, emptyAt: empty)])
    // Not again while it lasts, nor when it comes back within the window's cooldown.
    #expect(alerter.check([pool(shortEmpty: empty)], now: start.addingTimeInterval(400)).isEmpty)
    #expect(alerter.check([pool(shortEmpty: nil)], now: start.addingTimeInterval(500)).isEmpty)
    #expect(alerter.check([pool(shortEmpty: empty)], now: start.addingTimeInterval(600)).isEmpty)
    #expect(alerter.check([pool(shortEmpty: empty)], now: start.addingTimeInterval(900)).isEmpty)
    // After the cooldown it may alert again, and stale data never does.
    let later = start.addingTimeInterval(6 * 3600)
    #expect(alerter.check([pool(shortEmpty: empty, age: 3600)], now: later).isEmpty)
    #expect(alerter.check([pool(shortEmpty: empty)], now: later.addingTimeInterval(30)).isEmpty)
    #expect(alerter.check([pool(shortEmpty: empty)], now: later.addingTimeInterval(200)).count == 1)
    // The last alert times survive a restart.
    var restarted = PoolAlerter(lastSent: alerter.lastSent)
    #expect(restarted.check([pool(shortEmpty: empty)], now: later.addingTimeInterval(300)).isEmpty)
    #expect(restarted.check([pool(shortEmpty: empty)], now: later.addingTimeInterval(500)).isEmpty)
}

@Test func idleAccountCountsInTheFiveHourPool() throws {
    // One seat is busy in its 5h window, the other has never opened one: its points carry no
    // 5h fields. The pool is two windows wide, so 60% on one seat is 30% of the pool.
    let now = Date(timeIntervalSince1970: 100_000)
    let points = [
        HistoryPoint(t: 99_000, auth: "claude-1-busy@x.json", long_used: 0.3, long_reset: 500_000,
                     short_used: 0.5, short_reset: 110_000),
        HistoryPoint(t: 99_900, auth: "claude-1-busy@x.json", long_used: 0.3, long_reset: 500_000,
                     short_used: 0.6, short_reset: 110_000),
        HistoryPoint(t: 99_900, auth: "claude-1-idle@x.json", long_used: 0.1, long_reset: 600_000),
    ]
    let pool = try #require(QuotaModel.pools(points: points, activeAuths: Set(points.map(\.auth)), now: now).first)
    let short = try #require(pool.short)
    #expect(short.used == 30)
    #expect(pool.accounts.map { $0.short?.used } == [60, 0])
    // A provider whose accounts never report a 5h window has no 5h pool.
    let codex = [HistoryPoint(t: 99_900, auth: "codex-1-a@x.json", provider: "codex", long_used: 0.1, long_reset: 600_000)]
    #expect(QuotaModel.pools(points: codex, activeAuths: ["codex-1-a@x.json"], now: now).first?.short == nil)
}
