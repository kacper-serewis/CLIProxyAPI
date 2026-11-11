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
        for (n, w) in [("short", p.short), ("long", p.long)] {
            if let w { print("  ", n, String(format: "used %.1f burn %.2f sust %.2f pace %.2f lowest %.1f", w.used, w.burnPerHour, w.sustainablePerHour, w.pace, w.lowestLeft), w.emptyAt as Any, w.level, w.learned ? "learned" : "constant") }
        }
        for a in p.accounts {
            print("  ", a.name, a.nextChat ? "[next]" : "",
                  "5h", a.short.map { String(format: "%.0f%% burn %.1f proj %@ reset %@", $0.used, $0.burnPerHour, "\($0.projectedAtReset ?? -1)", "\($0.resetAt as Any)") } ?? "-",
                  "| wk", a.long.map { String(format: "%.0f%% burn %.2f proj %.1f el %.2f", $0.used, $0.burnPerHour, $0.projectedAtReset ?? -1, $0.elapsed ?? -1) } ?? "-")
        }
    }
}
