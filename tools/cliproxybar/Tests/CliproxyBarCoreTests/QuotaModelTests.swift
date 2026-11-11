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
    #expect(abs(late(now.addingTimeInterval(3600)) - 2) < 1e-9)
    #expect(late(date(12, 3)) < 0.2)
    #expect(abs(late(date(13, 13)) - 1) < 0.05)
    // Twice the usual pace at 13:00 scales the rhythm and fades.
    let busy = QuotaModel.projectedRate(profile, recentBurn: 2, recentHours: 1, now: date(11, 13), calendar: utc)
    #expect(abs(busy(date(11, 16)) - 1.5 * profile.weekday[16]) < 1e-9)
    #expect(busy(date(12, 3)) == 0)

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
