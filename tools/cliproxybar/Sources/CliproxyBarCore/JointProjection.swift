import Foundation

public struct HeadroomGap: Sendable {
    public var start: Date
    public var end: Date?
}

public struct HeadroomForecast: Sendable {
    public var gaps: [HeadroomGap]
    public var horizon: Date
    public var shortLowest: Double
    public var longLowest: Double
    public var shortAtReset: [Double?]
    public var longAtReset: [Double?]
    public var firstGap: HeadroomGap? { gaps.first }
}

extension QuotaModel {
    /// Keep in step with the panel's jointQuotaProjection.ts. One workload consumes both
    /// windows on a seat. Preserve observed workload shares until a safety threshold,
    /// then move that share to the soonest weekly reset. No backlog during a quota gap.
    public static func projectJoint(_ accounts: [AccountState], longRate: (Date) -> Double,
                                    shortRate: (Date) -> Double, now: Date,
                                    horizon: TimeInterval = longPeriod) -> HeadroomForecast {
        struct Seat {
            var longUsed: Double
            var shortUsed: Double?
            var longReset: Date
            var shortReset: Date
            var longPeriod: TimeInterval
            var shortPeriod: TimeInterval
            var share: Double
        }
        let shortBurn = accounts.reduce(0.0) { $0 + ($1.short?.burnPerHour ?? 0) }
        var seats = accounts.map { a in
            Seat(longUsed: max(a.long?.used ?? 0, a.longHighWaterUsed ?? 0),
                 shortUsed: a.short.map { max($0.used, a.shortHighWaterUsed ?? 0) },
                 longReset: a.long?.resetAt ?? .distantFuture, shortReset: a.short?.resetAt ?? .distantFuture,
                 longPeriod: a.long?.period ?? longPeriod, shortPeriod: a.short?.period ?? shortPeriod,
                 share: max(0, shortBurn > 0 ? a.short?.burnPerHour ?? 0 : a.long?.burnPerHour ?? 0))
        }
        let sum = seats.reduce(0) { $0 + $1.share }
        if sum > 0 { for i in seats.indices { seats[i].share /= sum } }
        let end = now.addingTimeInterval(horizon)
        var result = HeadroomForecast(gaps: [], horizon: end, shortLowest: 100, longLowest: 100,
                                      shortAtReset: Array(repeating: nil, count: seats.count),
                                      longAtReset: Array(repeating: nil, count: seats.count))
        guard !seats.isEmpty else { return result }
        let epsilon = 1e-8
        func eligible(_ i: Int) -> Bool {
            seats[i].longUsed < newChatLongLimit - epsilon && (seats[i].shortUsed ?? 0) < newChatShortLimit - epsilon
        }
        func record(_ time: Date) {
            result.longLowest = min(result.longLowest, seats.reduce(0) { $0 + 100 - $1.longUsed } / Double(seats.count))
            let shorts = seats.compactMap(\.shortUsed)
            if !shorts.isEmpty && time <= now.addingTimeInterval(shortPeriod) { result.shortLowest = min(result.shortLowest, shorts.reduce(0) { $0 + 100 - $1 } / Double(shorts.count)) }
        }
        var t = now
        record(t)
        while t < end {
            for i in seats.indices {
                if seats[i].longReset <= t {
                    if result.longAtReset[i] == nil { result.longAtReset[i] = seats[i].longUsed }
                    seats[i].longUsed = 0
                    seats[i].longReset = t.addingTimeInterval(seats[i].longPeriod)
                }
                if seats[i].shortReset <= t {
                    if result.shortAtReset[i] == nil { result.shortAtReset[i] = seats[i].shortUsed }
                    seats[i].shortUsed = 0
                    seats[i].shortReset = .distantFuture
                }
            }
            let order = seats.indices.filter { eligible($0) }.sorted {
                seats[$0].longReset == seats[$1].longReset ? $0 < $1 : seats[$0].longReset < seats[$1].longReset
            }
            let nextReset = seats.map { min($0.longReset, $0.shortReset) }.min() ?? end
            if order.isEmpty {
                if result.gaps.isEmpty || result.gaps.last?.end != nil { result.gaps.append(HeadroomGap(start: t, end: nil)) }
                t = min(end, nextReset)
                record(t)
                continue
            }
            if !result.gaps.isEmpty, result.gaps.last?.end == nil { result.gaps[result.gaps.count - 1].end = t }
            var weights = seats.indices.map { eligible($0) ? seats[$0].share : 0 }
            weights[order[0]] += max(0, 1 - weights.reduce(0, +))
            let next = min(t.addingTimeInterval(300), end, nextReset)
            let mid = t.addingTimeInterval(next.timeIntervalSince(t) / 2)
            let longBurn = max(0, longRate(mid)), shortBurn = max(0, shortRate(mid))
            var dt = next.timeIntervalSince(t)
            for i in order where weights[i] > 0 {
                if longBurn > 0 { dt = min(dt, (newChatLongLimit - seats[i].longUsed) / (longBurn * weights[i]) * 3600) }
                if shortBurn > 0, let used = seats[i].shortUsed { dt = min(dt, (newChatShortLimit - used) / (shortBurn * weights[i]) * 3600) }
            }
            for i in order where weights[i] > 0 {
                if seats[i].shortUsed != nil, seats[i].shortReset == .distantFuture, longBurn > 0 || shortBurn > 0 {
                    seats[i].shortReset = t.addingTimeInterval(seats[i].shortPeriod)
                }
                seats[i].longUsed += longBurn * weights[i] * dt / 3600
                if seats[i].shortUsed != nil { seats[i].shortUsed! += shortBurn * weights[i] * dt / 3600 }
            }
            t.addTimeInterval(dt)
            record(t)
        }
        return result
    }
}
