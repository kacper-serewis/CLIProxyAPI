import AppKit
import CliproxyBarCore
import SwiftUI

@main
struct CliproxyBarApp: App {
    @State private var store = Store()

    var body: some Scene {
        MenuBarExtra {
            PopoverView(store: store)
        } label: {
            MenuLabel(store: store)
        }
        .menuBarExtraStyle(.window)
    }
}

@MainActor @Observable
final class Store {
    var pools: [PoolState] = []
    var updated = Date()
    private var timer: Timer?

    init() {
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: 30, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.refresh() }
        }
    }

    func refresh() {
        updated = Date()
        pools = ProxyFiles.load(now: updated)
    }

    var claude: PoolState? { pools.first { $0.provider == "claude" } ?? pools.first }
}

extension Level {
    var color: Color {
        switch self {
        case .ok: .green
        case .watch: .orange
        case .over: .red
        case .stale: .gray
        }
    }
    var nsColor: NSColor {
        switch self {
        case .ok: .systemGreen
        case .watch: .systemOrange
        case .over: .systemRed
        case .stale: .systemGray
        }
    }
}

struct MenuLabel: View {
    let store: Store

    var body: some View {
        let pool = store.claude
        let stale = pool == nil || pool?.level == .stale
        Image(nsImage: Self.gauge([
            (pool?.short?.used ?? 0, stale ? .stale : pool?.short?.level ?? .stale),
            (pool?.long?.used ?? 0, stale ? .stale : pool?.long?.level ?? .stale),
        ]))
    }

    /// Two stacked mini bars: 5h on top, weekly below. Fill = pool used, colour = verdict.
    static func gauge(_ bars: [(used: Double, level: Level)]) -> NSImage {
        let width: CGFloat = 18, barHeight: CGFloat = 4, gap: CGFloat = 2
        let height = CGFloat(bars.count) * barHeight + CGFloat(bars.count - 1) * gap
        let image = NSImage(size: NSSize(width: width, height: height), flipped: true) { _ in
            for (i, bar) in bars.enumerated() {
                let y = CGFloat(i) * (barHeight + gap)
                let track = NSRect(x: 0, y: y, width: width, height: barHeight)
                bar.level.nsColor.withAlphaComponent(0.3).setFill()
                NSBezierPath(roundedRect: track, xRadius: 2, yRadius: 2).fill()
                let fill = max(barHeight, width * min(1, max(0, bar.used / 100)))
                bar.level.nsColor.setFill()
                NSBezierPath(roundedRect: NSRect(x: 0, y: y, width: fill, height: barHeight),
                             xRadius: 2, yRadius: 2).fill()
            }
            return true
        }
        image.isTemplate = false
        return image
    }
}

func pct(_ value: Double?) -> String {
    guard let value else { return "–" }
    return "\(Int(value.rounded()))%"
}
