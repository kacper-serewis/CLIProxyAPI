import Foundation
import Testing
@testable import CliproxyBarCore

@Test(.enabled(if: ProcessInfo.processInfo.environment["CPB_LIVE"] != nil)) func dumpLive() {
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
