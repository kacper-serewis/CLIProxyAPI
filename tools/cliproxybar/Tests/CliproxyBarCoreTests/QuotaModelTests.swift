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
