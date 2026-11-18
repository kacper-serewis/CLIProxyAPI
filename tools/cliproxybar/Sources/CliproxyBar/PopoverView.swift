import AppKit
import CliproxyBarCore
import SwiftUI

struct PopoverView: View {
    let store: Store

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            if store.pools.isEmpty {
                Text("No quota history found in ~/.cli-proxy-api.").foregroundStyle(.secondary)
            }
            ForEach(store.pools, id: \.provider) { pool in
                PoolSection(pool: pool, now: store.updated)
                if pool.provider != store.pools.last?.provider { Divider() }
            }
            Divider()
            HStack {
                Text("Updated \(store.updated.formatted(date: .omitted, time: .shortened))")
                    .font(.caption).foregroundStyle(.secondary)
                Spacer()
                Button("Refresh") { store.refresh() }
                Button("Panel") {
                    NSWorkspace.shared.open(URL(string: "http://127.0.0.1:8317/management.html")!)
                }
                Button("Quit") { NSApp.terminate(nil) }
            }
            .controlSize(.small)
        }
        .padding(14)
        .frame(width: 360)
    }
}

struct PoolSection: View {
    let pool: PoolState
    let now: Date

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            HStack(spacing: 6) {
                Circle().fill(pool.level.color).frame(width: 9, height: 9)
                Text(pool.provider.capitalized + " pool").font(.headline)
                Text("\(pool.accounts.count) account\(pool.accounts.count == 1 ? "" : "s")")
                    .foregroundStyle(.secondary)
                Spacer()
                Text(headline).font(.subheadline.weight(.medium)).foregroundStyle(pool.level.color)
            }
            if pool.level == .stale {
                Text("Last sample \(duration(pool.dataAge)) ago — is the proxy running?")
                    .font(.caption).foregroundStyle(.secondary)
            }
            if pool.level != .stale, let gap = pool.forecast?.firstGap, !pool.availability.needsAttention {
                VStack(alignment: .leading, spacing: 4) {
                    Label("Headroom runs out \(when(gap.start, now: now))", systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(.red)
                    if let reset = gap.end { Text("Returns \(when(reset, now: now)).") }
                    Text("The forecast spends both quota windows on the same account. Fallback quota may still work.")
                        .foregroundStyle(.secondary)
                }.font(.caption)
            }
            if pool.availability.needsAttention {
                VStack(alignment: .leading, spacing: 4) {
                    Label(pool.availability == .exhausted ? "Every account has a full quota window."
                          : "Every account is near or at a quota limit.", systemImage: "exclamationmark.triangle.fill")
                        .foregroundStyle(.red)
                    if pool.availability == .constrained {
                        Text("The proxy may still try the remaining quota, but no seat has safe headroom in both windows.")
                    }
                    if let reset = pool.headroomAt {
                        Text("Headroom returns \(when(reset, now: now)), assuming no further usage.")
                    }
                    Text("The forecast spends both windows together. Spare quota on different seats cannot be combined.")
                        .foregroundStyle(.secondary)
                }
                .font(.caption)
            }
            if let short = pool.short {
                PoolWindowView(title: "5-hour", window: short, now: now,
                               burnLabel: "last hour", detail: nil,
                               rhythmLead: "At this pace and your usual rhythm")
            }
            if let long = pool.long {
                PoolWindowView(title: "Weekly", window: long, now: now,
                               burnLabel: long.learned ? "usual week" : "24h avg",
                               detail: String(format: "last 5h: %.1f pts/h", pool.longBurnRecent))
            }
            ForEach(pool.accounts) {
                AccountView(account: $0, now: now,
                            shortPool: pool.headroomWarning ? .over : pool.short?.level ?? pool.level,
                            longPool: pool.headroomWarning ? .over : pool.long?.level ?? pool.level)
            }
        }
    }

    private var headline: String {
        if pool.availability == .exhausted { return "Quota exhausted" }
        if pool.availability == .constrained { return "No safe headroom" }
        if pool.headroomWarning { return "Headroom running out" }
        return switch pool.level {
        case .ok: "Within limits"
        case .watch: "Getting tight"
        case .over: "Over pace"
        case .stale: "No data"
        }
    }
}

struct PoolWindowView: View {
    let title: String
    let window: PoolWindow
    let now: Date
    let burnLabel: String
    let detail: String?
    var rhythmLead = "At your usual rhythm"

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack {
                Text(title).font(.subheadline.weight(.semibold))
                Spacer()
                Text("\(pct(window.used)) used").monospacedDigit()
            }
            UsageBar(used: window.used, marker: nil, level: window.level)
            Text(verdict).font(.caption).foregroundStyle(window.level == .ok ? .secondary : window.level.color)
            Text(burn).font(.caption).foregroundStyle(.secondary)
        }
    }

    private var verdict: String {
        if let empty = window.emptyAt {
            return "\(window.learned ? rhythmLead : "At this pace") headroom runs out \(when(empty, now: now))"
        }
        return "Lowest projected quota — \(pct(window.lowestLeft)) left"
    }

    private var burn: String {
        var s = String(format: "Burn %.1f pts/h (%@) · %.1f× sustainable", window.burnPerHour, burnLabel, window.pace)
        if let detail { s += " · " + detail }
        return s
    }
}

struct AccountView: View {
    let account: AccountState
    let now: Date
    /// The pool's verdict for each window: a row is never more alarming than its pool.
    let shortPool: Level
    let longPool: Level

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                Text(account.name).font(.callout.weight(.medium))
                if account.nextChat {
                    Text("next chat").font(.caption2.weight(.semibold))
                        .padding(.horizontal, 5).padding(.vertical, 1)
                        .background(Capsule().fill(Color.accentColor.opacity(0.2)))
                }
                Spacer()
            }
            if account.constrained {
                Text("\(account.shortConstrained && account.longConstrained ? "5h + weekly" : account.shortConstrained ? "5h" : "Weekly") quota \(account.exhausted ? "at limit" : "near limit")")
                    .font(.caption2).foregroundStyle(shortPool == .over || longPool == .over ? .red : .secondary)
            }
            if account.reserved && !account.nextChat {
                Text("Kept for when the other seat is full").font(.caption2).foregroundStyle(.secondary)
            }
            if let s = account.short { row("5h", s, pool: shortPool) }
            if let l = account.long { row("Week", l, pool: longPool) }
        }
        .padding(8)
        .background(RoundedRectangle(cornerRadius: 8).fill(Color.primary.opacity(0.05)))
    }

    private func row(_ label: String, _ w: WindowState, pool: Level) -> some View {
        let projected = w.projectedAtReset
        let level = QuotaModel.accountLevel(w, pool: pool)
        return VStack(alignment: .leading, spacing: 2) {
            HStack(spacing: 6) {
                Text(label).font(.caption).frame(width: 34, alignment: .leading)
                UsageBar(used: w.used, marker: w.elapsed.map { $0 * 100 }, level: level)
                Text(pct(w.used)).font(.caption).monospacedDigit().frame(width: 36, alignment: .trailing)
            }
            HStack {
                Text(w.resetAt.map { "resets \(when($0, now: now))" } ?? "idle — starts on next use")
                Spacer()
                if let projected, projected >= w.used + 0.5 {
                    // 100% with a healthy pool means the seat gets used up, not that work stops.
                    Text(projected >= 100 ? "→ full before reset" : "→ \(pct(projected)) at reset")
                        .foregroundStyle(level == .ok ? .secondary : level.color)
                }
            }
            .font(.caption2).foregroundStyle(.secondary)
            .padding(.leading, 40)
        }
    }
}

/// Usage bar; the tick marks how much of the window has passed (on pace = fill reaches the tick).
struct UsageBar: View {
    let used: Double
    let marker: Double?
    let level: Level

    var body: some View {
        GeometryReader { geo in
            ZStack(alignment: .leading) {
                Capsule().fill(Color.primary.opacity(0.1))
                Capsule().fill(level.color.opacity(0.85))
                    .frame(width: max(0, min(1, used / 100)) * geo.size.width)
                if let marker {
                    Rectangle().fill(Color.primary.opacity(0.6))
                        .frame(width: 1.5, height: geo.size.height + 4)
                        .offset(x: max(0, min(1, marker / 100)) * geo.size.width - 0.75)
                }
            }
        }
        .frame(height: 6)
    }
}

func when(_ date: Date, now: Date) -> String {
    let dt = date.timeIntervalSince(now)
    let clock = date.formatted(date: .omitted, time: .shortened)
    if dt < 0 { return "now" }
    if dt < 12 * 3600 { return "in \(duration(dt)) (\(clock))" }
    let day = date.formatted(.dateTime.weekday(.abbreviated))
    return "\(day) \(clock) (in \(duration(dt)))"
}

func duration(_ seconds: TimeInterval) -> String {
    let m = Int(seconds / 60)
    if m < 60 { return "\(m)m" }
    if m < 48 * 60 { return "\(m / 60)h \(m % 60)m" }
    return "\(m / 1440)d \((m % 1440) / 60)h"
}
