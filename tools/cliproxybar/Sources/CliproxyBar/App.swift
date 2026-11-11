import AppKit
import CliproxyBarCore
import SwiftUI
import UserNotifications

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
    private var alerter = PoolAlerter(lastSent: Notifier.loadLastSent())

    init() {
        // Asking before the app has finished launching is refused outright.
        Task { @MainActor in
            try? await Task.sleep(for: .seconds(2))
            Notifier.setUp()
        }
        refresh()
        timer = Timer.scheduledTimer(withTimeInterval: 30, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.refresh() }
        }
    }

    func refresh() {
        updated = Date()
        pools = ProxyFiles.load(now: updated)
        let alerts = alerter.check(pools, now: updated)
        if !alerts.isEmpty {
            Notifier.saveLastSent(alerter.lastSent)
            for alert in alerts { Notifier.post(alert, now: updated) }
        }
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

/// macOS notifications for a pool that is projected to run dry. Needs the app bundle (the
/// installed app); under `swift run` there is no bundle and nothing is posted. Do not change
/// the bundle identifier lightly: the user's permission hangs on it.
@MainActor
enum Notifier {
    private static let defaultsKey = "poolAlertsLastSent"
    private static let delegate = Delegate()
    private static var available: Bool { Bundle.main.bundleIdentifier != nil }

    static func setUp() {
        guard available else { return }
        let center = UNUserNotificationCenter.current()
        center.delegate = delegate
        let args = CommandLine.arguments
        // macOS only asks the user when the app was opened the normal way (`open`), not when
        // launchd starts the binary. So install.sh opens it once with --request-notifications:
        // ask, then quit. --test-notification also posts a sample first.
        let oneShot = args.contains("--request-notifications") || args.contains("--test-notification")
        if oneShot {
            // Nobody answering the prompt must not leave a second copy running.
            Task { @MainActor in
                try? await Task.sleep(for: .seconds(60))
                exit(2)
            }
        }
        center.requestAuthorization(options: [.alert, .sound]) { granted, error in
            if let error { NSLog("CliproxyBar: notifications not authorised: \(error.localizedDescription)") }
            guard oneShot else { return }
            print("notifications allowed: \(granted)\(error.map { " (\($0.localizedDescription))" } ?? "")")
            Task { @MainActor in
                if granted, args.contains("--test-notification") {
                    let now = Date()
                    post(PoolAlert(key: "test", provider: "claude", short: true, emptyAt: now.addingTimeInterval(4800)), now: now)
                    try? await Task.sleep(for: .seconds(3))
                }
                exit(granted ? 0 : 1)
            }
        }
    }

    static func post(_ alert: PoolAlert, now: Date) {
        guard available else { return }
        let content = UNMutableNotificationContent()
        let window = alert.short ? "5-hour" : "weekly"
        content.title = "\(alert.provider.capitalized) \(window) pool is running out"
        content.body = "At this pace it is empty \(when(alert.emptyAt, now: now)). "
            + (alert.short ? "Slow down or take a break until the next reset." : "Spread the work or it runs dry before the reset.")
        content.sound = .default
        UNUserNotificationCenter.current().add(
            UNNotificationRequest(identifier: alert.key, content: content, trigger: nil)) { error in
            if let error { NSLog("CliproxyBar: notification failed: \(error.localizedDescription)") }
        }
    }

    static func loadLastSent() -> [String: Date] {
        let stored = UserDefaults.standard.dictionary(forKey: defaultsKey) as? [String: Double] ?? [:]
        return stored.mapValues { Date(timeIntervalSince1970: $0) }
    }

    static func saveLastSent(_ lastSent: [String: Date]) {
        UserDefaults.standard.set(lastSent.mapValues(\.timeIntervalSince1970), forKey: defaultsKey)
    }

    /// Show the banner even while the popover has focus.
    private final class Delegate: NSObject, UNUserNotificationCenterDelegate {
        func userNotificationCenter(_ center: UNUserNotificationCenter,
                                    willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
            [.banner, .sound]
        }
    }
}
