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

    /// The gaps that are going on or still to come, each with its length and how much of it
    /// falls in working hours. One that began earlier counts from `now`; one without an end
    /// runs to the horizon and lasts at least that long.
    public func events(now: Date, calendar: Calendar = .current) -> [HeadroomEvent] {
        gaps.filter { $0.end.map { $0 > now } ?? true }.map { gap in
            let start = max(gap.start, now), end = gap.end ?? max(horizon, start)
            return HeadroomEvent(start: start, end: end, ended: gap.end != nil,
                                 working: QuotaModel.workingTime(from: start, to: end, calendar: calendar))
        }
    }
}

/// One expected period without safe headroom.
public struct HeadroomEvent: Sendable, Equatable {
    public var start: Date
    public var end: Date
    /// False when headroom does not return within the forecast.
    public var ended: Bool
    /// How much of it falls in working hours.
    public var working: TimeInterval
    public var length: TimeInterval { end.timeIntervalSince(start) }
}

extension QuotaModel {
    /// Working hours, local time on weekdays: where a gap hurts. Same as the panel's list.
    public static let workStartMinutes = 9 * 60 + 30
    public static let workEndMinutes = 17 * 60 + 30

    /// The part of a period that falls in working hours.
    public static func workingTime(from start: Date, to end: Date, calendar: Calendar = .current) -> TimeInterval {
        var total: TimeInterval = 0
        var day = calendar.startOfDay(for: start)
        while day < end {
            let weekday = calendar.component(.weekday, from: day)
            if weekday >= 2 && weekday <= 6,
               let from = calendar.date(byAdding: .minute, value: workStartMinutes, to: day),
               let to = calendar.date(byAdding: .minute, value: workEndMinutes, to: day) {
                total += max(0, min(end, to).timeIntervalSince(max(start, from)))
            }
            guard let next = calendar.date(byAdding: .day, value: 1, to: day) else { break }
            day = next
        }
        return total
    }
}

extension QuotaModel {
    /// Keep in step with the panel's jointQuotaProjection.ts. One workload consumes both
    /// windows on a seat. Preserve observed workload shares until a safety threshold,
    /// then move that share to the soonest weekly reset. A seat in its weekly reserve hands
    /// its share over too and only works when no other seat has headroom (chats already on
    /// it are assumed to finish soon). No backlog during a quota gap.
    public static func projectJoint(_ accounts: [AccountState], longRate: @escaping (Date) -> Double,
                                    shortRate: (Date) -> Double, now: Date,
                                    horizon: TimeInterval = longPeriod, reserve: Bool = true) -> HeadroomForecast {
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
        let expected = ExpectedUsage(rate: longRate, now: now)
        func reserved(_ i: Int, at time: Date) -> Bool {
            reserve && inReserve(longUsed: seats[i].longUsed, reset: seats[i].longReset, at: time, expected: expected)
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
            let open = seats.indices.filter { eligible($0) }
            let free = open.filter { !reserved($0, at: t) }
            // Reserved seats take the overflow only when no other seat has headroom.
            let order = (free.isEmpty ? open : free).sorted {
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
            var weights = seats.indices.map { order.contains($0) ? seats[$0].share : 0 }
            weights[order[0]] += max(0, 1 - weights.reduce(0, +))
            let next = min(t.addingTimeInterval(300), end, nextReset)
            let mid = t.addingTimeInterval(next.timeIntervalSince(t) / 2)
            let longBurn = max(0, longRate(mid)), shortBurn = max(0, shortRate(mid))
            var dt = next.timeIntervalSince(t)
            for i in order where weights[i] > 0 {
                if longBurn > 0 {
                    // Stop where the seat enters its reserve, so that is decided at the threshold.
                    let limit = reserve && seats[i].longUsed < longReserveLimit - epsilon ? longReserveLimit : newChatLongLimit
                    dt = min(dt, (limit - seats[i].longUsed) / (longBurn * weights[i]) * 3600)
                }
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
