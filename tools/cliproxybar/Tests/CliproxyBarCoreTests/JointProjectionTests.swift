import Foundation
import Testing
@testable import CliproxyBarCore

private let start = Date(timeIntervalSince1970: 100_000)
private func seat(_ id: String, long: Double, short: Double?, longReset: Double,
                  shortReset: Double?, burn: Double = 0) -> AccountState {
    AccountState(id: id, name: id, provider: "claude",
                 short: short.map { WindowState(used: $0, resetAt: shortReset.map { start.addingTimeInterval($0 * 3600) },
                                                period: 5 * 3600, burnPerHour: burn) },
                 long: WindowState(used: long, resetAt: start.addingTimeInterval(longReset * 3600),
                                   period: 168 * 3600, burnPerHour: burn / 10), observedAt: start)
}

@Test func jointForecastPredictsSplitLimitsBeforeEitherPoolEmpties() throws {
    let accounts = [seat("a", long: 98, short: 0, longReset: 5, shortReset: nil),
                    seat("b", long: 65, short: 80, longReset: 72, shortReset: 2, burn: 20)]
    let forecast = QuotaModel.projectJoint(accounts, longRate: { _ in 2 }, shortRate: { _ in 20 },
                                          now: start, horizon: 3 * 3600)
    let gap = try #require(forecast.firstGap)
    #expect(abs(gap.start.timeIntervalSince(start) - 3600) < 0.01)
    #expect(gap.end == start.addingTimeInterval(7200))
    #expect(forecast.longLowest > 0)
    #expect(forecast.shortLowest > 0)
    // The former separate projections would both claim this workload lasts three hours.
    for short in [true, false] {
        let windows = accounts.compactMap { short ? $0.short : $0.long }
        let sim = QuotaModel.simulate(windows.map { QuotaModel.SimAccount(remaining: 100 - $0.used,
            resetAt: $0.resetAt ?? start.addingTimeInterval($0.period), period: $0.period) },
            burnPerHour: short ? 20 : 2, now: start, horizon: 3 * 3600)
        #expect(sim.emptyAt == nil)
    }
    #expect(forecast.shortAtReset[1] == 90)
}

@Test func jointForecastHonoursResetsInsideAStepAndBothLimits() throws {
    // b resets 90 seconds into a step, before its remaining headroom would run out.
    let accounts = [seat("a", long: 99, short: 0, longReset: 5, shortReset: nil),
                    seat("b", long: 50, short: 89, longReset: 72, shortReset: 0.025, burn: 20)]
    let forecast = QuotaModel.projectJoint(accounts, longRate: { _ in 2 }, shortRate: { _ in 20 },
                                          now: start, horizon: 1800)
    #expect(forecast.gaps.isEmpty)
    #expect(abs((forecast.shortAtReset[1] ?? 0) - 89.5) < 1e-8)
    let blocked = [seat("a", long: 99, short: 90, longReset: 3, shortReset: 1),
                   seat("b", long: 50, short: 90, longReset: 72, shortReset: 2)]
    let gap = try #require(QuotaModel.projectJoint(blocked, longRate: { _ in 0 }, shortRate: { _ in 0 },
        now: start, horizon: 4 * 3600).firstGap)
    #expect(gap.start == start)
    #expect(gap.end == start.addingTimeInterval(7200))
}

@Test func idleWindowStartsWhenItsWorkloadStartsAndNoShortWindowIsSupported() throws {
    let accounts = [seat("a", long: 0, short: 0, longReset: 100, shortReset: nil)]
    let forecast = QuotaModel.projectJoint(accounts, longRate: { $0 < start.addingTimeInterval(7200) ? 0 : 1 },
        shortRate: { $0 < start.addingTimeInterval(7200) ? 0 : 10 }, now: start, horizon: 8 * 3600)
    #expect(forecast.gaps.isEmpty)
    #expect(abs((forecast.shortAtReset[0] ?? 0) - 50) < 1e-8)
    let weekly = [seat("codex", long: 98, short: nil, longReset: 10, shortReset: nil)]
    let gap = try #require(QuotaModel.projectJoint(weekly, longRate: { _ in 1 }, shortRate: { _ in 0 },
        now: start, horizon: 2 * 3600).firstGap)
    #expect(abs(gap.start.timeIntervalSince(start) - 3600) < 0.01)
    #expect(gap.end == nil)
}

@Test func forecastWarnsAndNotifiesBeforeCurrentHeadroomIsLost() throws {
    let accounts = [seat("a", long: 98, short: 0, longReset: 5, shortReset: nil),
                    seat("b", long: 65, short: 80, longReset: 72, shortReset: 2, burn: 20)]
    let forecast = QuotaModel.projectJoint(accounts, longRate: { _ in 2 }, shortRate: { _ in 20 },
                                          now: start, horizon: 3 * 3600)
    var pool = PoolState(provider: "claude", accounts: accounts, longBurnRecent: 2, dataAge: 0, forecast: forecast)
    #expect(pool.availability == .available)
    #expect(pool.headroomWarning)
    #expect(pool.level == .over)
    var alerter = PoolAlerter()
    #expect(alerter.check([pool], now: start).isEmpty)
    let alerts = alerter.check([pool], now: start.addingTimeInterval(120))
    #expect(alerts.count == 1)
    let alert = try #require(alerts.first)
    #expect(alert.joint)
    #expect(alert.emptyAt > start.addingTimeInterval(120))
    #expect(alert.recoversAt == start.addingTimeInterval(7200))
    #expect(alerter.check([pool], now: start.addingTimeInterval(240)).isEmpty)
    pool.dataAge = QuotaModel.staleAfter + 1
    #expect(!pool.headroomWarning)
    #expect(pool.level == .stale)
}
