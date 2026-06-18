import Foundation
import Observation

enum ConnectionState: Equatable {
    case connecting
    case connected
    case disconnected(reason: String)

    /// Presentation strings for the disconnected/connecting panel, kept next to the
    /// state so the view stays a thin render layer.
    var disconnectedTitle: String {
        self == .connecting ? "Connecting…" : "Broker offline"
    }

    var disconnectedDetail: String {
        if case .disconnected(let reason) = self { return reason }
        return "Reaching the daemon…"
    }
}

struct BuildSummary: Equatable {
    let building: Int
    let queued: Int
}

/// Capped exponential backoff for WS reconnects.
struct Backoff {
    let minSeconds: Double
    let maxSeconds: Double
    private var current: Double

    init(min: Double, max: Double) {
        self.minSeconds = min
        self.maxSeconds = max
        self.current = min
    }

    mutating func next() -> Double {
        let value = current
        current = Swift.min(current * 2, maxSeconds)
        return value
    }

    mutating func reset() { current = minSeconds }
}

/// The single observable snapshot the SwiftUI views render. A pure projection of
/// broker state: an initial `GET /builds`, then WS `snapshot`/`build` events applied
/// to it. No persistence, no business logic — the kill button POSTs an intent and
/// waits for the daemon's authoritative `build` event rather than mutating locally.
@MainActor
@Observable
final class BrokerStore {
    private(set) var builds: [Build] = []
    private(set) var connection: ConnectionState = .connecting

    private var client: BrokerClient?
    private var loopTask: Task<Void, Never>?
    private var backoff = Backoff(min: 0.5, max: 30)

    /// Sorted view for the menu: active builds first, then most-recently-started.
    var sortedBuilds: [Build] {
        builds.sorted { a, b in
            if a.isActive != b.isActive { return a.isActive }
            return a.startTime > b.startTime
        }
    }

    /// "N building, M queued" — `building` counts `.running` (the wire word for an
    /// active build), `queued` counts `.queued`.
    var summary: BuildSummary {
        let building = builds.filter { $0.state == .running }.count
        let queued = builds.filter { $0.state == .queued }.count
        return BuildSummary(building: building, queued: queued)
    }

    func start() {
        guard loopTask == nil else { return }
        reconnect()
    }

    /// Test/preview seam: inject a snapshot and connection state without networking.
    /// Used by the SwiftUI render snapshot and previews.
    func seed(builds: [Build], connection: ConnectionState) {
        self.builds = builds
        self.connection = connection
    }

    func reconnect() {
        loopTask?.cancel()
        loopTask = Task { [weak self] in await self?.connectLoop() }
    }

    private func connectLoop() async {
        while !Task.isCancelled {
            do {
                let cfg = try TokenLoader.load()
                let client = BrokerClient(config: cfg)
                self.client = client
                connection = .connecting

                let initial = try await client.fetchBuilds()
                builds = initial
                connection = .connected
                backoff.reset()

                // Live deltas until the socket errors/closes.
                for try await event in await client.events() {
                    apply(event: event)
                }
                // Clean close → reconnect after a backoff.
                connection = .disconnected(reason: "connection closed")
            } catch {
                connection = .disconnected(reason: Self.describe(error))
            }
            if Task.isCancelled { break }
            try? await Task.sleep(for: .seconds(backoff.next()))
        }
    }

    /// Fire-and-forget kill intent. The UI updates only when the daemon's `build`
    /// event (terminal state) arrives — no optimistic local mutation.
    func kill(_ invocationID: String) {
        Task { [weak self] in
            guard let client = self?.client else { return }
            try? await client.kill(invocationID: invocationID)
        }
    }

    // MARK: Reducers (pure, no I/O)

    private func apply(event: Event) {
        switch event.type {
        case .snapshot:
            if let snapshot = event.builds { builds = snapshot }
        case .build:
            if let build = event.build { upsert(build) }
        case .unknown:
            break   // reserved metrics/alert or a future type — ignored.
        }
    }

    /// Upsert by `invocation_id`. A terminal-state row stays visible (greyed) so the
    /// user sees the outcome; `sortedBuilds` sinks it below active builds.
    private func upsert(_ build: Build) {
        if let idx = builds.firstIndex(where: { $0.invocationID == build.invocationID }) {
            builds[idx] = build
        } else {
            builds.append(build)
        }
    }

    private static func describe(_ error: Error) -> String {
        switch error {
        case TokenLoaderError.missing, TokenLoaderError.unreadable:
            return "broker not configured"
        case TokenLoaderError.malformed:
            return "config unreadable"
        case BrokerClientError.http(let status) where status == 401:
            return "unauthorized (token mismatch)"
        default:
            return "broker offline"
        }
    }
}
