import Foundation
import Testing
@testable import CliproxyBarCore

@Test(.enabled(if: ProcessInfo.processInfo.environment["CPB_LIVE"] != nil)) func dumpLive() {
    let raw = QuotaModel.parse((try? Data(contentsOf: ProxyFiles.dir.appendingPathComponent("quota-history.jsonl"))) ?? Data())
    let active = ProxyFiles.activeAuths()
    for provider in ["claude", "codex"] {
        let lists = Dictionary(grouping: raw.filter { $0.provider == provider && active.contains($0.auth) }, by: \.auth)
            .values.map { $0.sorted { $0.t < $1.t } }
        print("5h points per weekly point", provider, QuotaModel.shortPerLongPoint(lists, now: Date()) as Any)
    }
    for p in ProxyFiles.load() {
        print("POOL", p.provider, "age", Int(p.dataAge), "level", p.level)
        print("  availability", p.availability, "headroom returns", p.headroomAt as Any)
        print("  first headroom gap", p.forecast?.firstGap as Any)
        let day = Date.FormatStyle.dateTime.weekday(.abbreviated).day().hour().minute()
        for e in p.forecast?.events(now: Date()) ?? [] {
            print("  gap", e.start.formatted(day), "–", e.end.formatted(day), e.ended ? "" : "(at least)",
                  Int(e.length / 60), "min,", Int(e.working / 60), "min in working hours")
        }
        for (n, w) in [("short", p.short), ("long", p.long), ("fable", p.fable)] {
            if let w { print("  ", n, String(format: "used %.1f burn %.2f sust %.2f pace %.2f lowest %.1f", w.used, w.burnPerHour, w.sustainablePerHour, w.pace, w.lowestLeft), w.emptyAt as Any, w.level, w.learned ? "learned" : "constant") }
        }
        for a in p.accounts {
            print("  ", a.name, a.nextChat ? "[next]" : "",
                  "5h", a.short.map { String(format: "%.0f%% burn %.1f proj %@ reset %@", $0.used, $0.burnPerHour, "\($0.projectedAtReset ?? -1)", "\($0.resetAt as Any)") } ?? "-",
                  "| wk", a.long.map { String(format: "%.0f%% burn %.2f proj %.1f el %.2f", $0.used, $0.burnPerHour, $0.projectedAtReset ?? -1, $0.elapsed ?? -1) } ?? "-",
                  "| fable", a.fable.map { String(format: "%.0f%% burn %.2f proj %.1f", $0.used, $0.burnPerHour, $0.projectedAtReset ?? -1) } ?? "-")
        }
    }
}


@Test(.enabled(if: ProcessInfo.processInfo.environment["CPB_LIVE"] != nil)) func dumpHeadroomReplay() {
    let raw = QuotaModel.parse((try? Data(contentsOf: ProxyFiles.dir.appendingPathComponent("quota-history.jsonl"))) ?? Data())
    let active = ProxyFiles.activeAuths()
    let current = Date()
    for minutes in [120, 90, 60, 30, 0] {
        let now = current.addingTimeInterval(Double(-minutes * 60))
        let points = raw.filter { $0.t <= now.timeIntervalSince1970 }
        if let pool = QuotaModel.pools(points: points, activeAuths: active, now: now).first(where: { $0.provider == "claude" }) {
            print("REPLAY", now, "availability", pool.availability, "first gap", pool.forecast?.firstGap as Any)
        }
    }
}
