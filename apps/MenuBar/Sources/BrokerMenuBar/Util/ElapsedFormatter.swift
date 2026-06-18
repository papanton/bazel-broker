import Foundation

/// Renders a build's elapsed time as "3m12s" / "48s" / "1h02m". Seeds from the
/// server's authoritative `elapsed_ms` (computed daemon-side, skew-free) and ticks
/// locally for running builds; terminal builds freeze at their fetched elapsed. This
/// matches `brokerctl`'s number exactly at fetch time.
enum ElapsedFormatter {
    static func string(for build: Build, now: Date = Date()) -> String {
        guard build.isActive, build.endTime == nil else {
            // Terminal builds freeze at the server's elapsed_ms.
            return format(millis: build.elapsedMS)
        }
        // Live builds tick: whichever is larger of the server seed or wall-time
        // since start_time (both non-negative for an active build).
        let seeded = Double(build.elapsedMS) / 1000.0
        let live = max(seeded, now.timeIntervalSince(build.startTime))
        return format(millis: Int64(live * 1000))
    }

    static func format(millis: Int64) -> String {
        let totalSeconds = Int(millis / 1000)
        let h = totalSeconds / 3600
        let m = (totalSeconds % 3600) / 60
        let s = totalSeconds % 60
        if h > 0 { return String(format: "%dh%02dm", h, m) }
        if m > 0 { return String(format: "%dm%02ds", m, s) }
        return "\(s)s"
    }
}
