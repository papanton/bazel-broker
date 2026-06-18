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

    /// Lifecycle of the broker daemon (a LaunchAgent the app ensures is running).
    private(set) var daemon: DaemonState = .offline
    /// Last daemon-action / cache-config message, shown transiently in the menu.
    private(set) var statusLine: String?

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
        loopTask = Task { [weak self] in await self?.bootstrapAndConnect() }
    }

    /// The single-entry-point bootstrap: ensure the daemon is running (start it as a
    /// LaunchAgent if it is not), then connect. Quitting the app never stops the daemon.
    private func bootstrapAndConnect() async {
        await ensureDaemonRunning()
        await connectLoop()
    }

    /// The daemon-owned config when present, else the fixed default-port config for the
    /// pre-config `/healthz` probe (the daemon writes config.json on its first run).
    private var probingConfig: BrokerConfig {
        (try? TokenLoader.load()) ?? BrokerConfig(port: 8765, token: "")
    }

    /// Probe `/healthz`; if unreachable, bootstrap the LaunchAgent and poll until ready.
    private func ensureDaemonRunning() async {
        if await DaemonController.probe(config: probingConfig) != nil {
            daemon = .running
            return
        }
        await bootstrapAgent(DaemonController.startAgent, failurePrefix: "broker start failed")
    }

    /// Shared agent-bootstrap step: run an install action, then poll `/healthz`. Both the
    /// launch path and the explicit "Restart Broker" action funnel through here.
    @discardableResult
    private func bootstrapAgent(_ action: () -> DaemonActionResult,
                                failurePrefix: String) async -> Bool {
        daemon = .starting
        let result = action()
        guard result.ok else {
            daemon = .failed(result.message)
            statusLine = "\(failurePrefix): \(result.message)"
            return false
        }
        let ready = await DaemonController.waitUntilReady(config: probingConfig)
        daemon = ready ? .running : .offline
        return ready
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
                daemon = .running
                backoff.reset()

                // Live deltas until the socket errors/closes.
                for try await event in await client.events() {
                    apply(event: event)
                }
                // Clean close → reconnect after a backoff.
                connection = .disconnected(reason: "connection closed")
            } catch {
                connection = .disconnected(reason: Self.describe(error))
                if daemon == .running { daemon = .offline }
            }
            if Task.isCancelled { break }
            try? await Task.sleep(for: .seconds(backoff.next()))
        }
    }

    // MARK: Daemon lifecycle actions (menu-driven)

    /// "Start Broker" — bootstrap the LaunchAgent, then connect.
    func startBroker() {
        statusLine = "starting broker…"
        loopTask?.cancel()
        loopTask = Task { [weak self] in await self?.bootstrapAndConnect() }
    }

    /// "Restart Broker" — bootout + bootstrap the LaunchAgent (an idempotent re-bootstrap
    /// via `startAgent`), then reconnect.
    func restartBroker() {
        statusLine = "restarting broker…"
        loopTask?.cancel()
        loopTask = Task { [weak self] in
            guard let self else { return }
            let ready = await self.bootstrapAgent(DaemonController.startAgent,
                                                   failurePrefix: "restart failed")
            self.statusLine = ready ? "broker restarted" : "broker did not come up"
            await self.connectLoop()
        }
    }

    /// "Stop Broker" — uninstall the LaunchAgent (the daemon stops and stays stopped).
    func stopBroker() {
        loopTask?.cancel()
        loopTask = nil
        Task { [weak self] in
            guard let self else { return }
            let result = DaemonController.stopAgent()
            self.daemon = .offline
            self.connection = .disconnected(reason: "broker stopped")
            self.builds = []
            self.statusLine = result.ok ? "broker stopped" : "stop failed: \(result.message)"
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

    // MARK: Cache config (E1) — apply to a user-chosen workspace

    /// Run the bundled `setup.sh <dir>` against the chosen workspace, surfacing the
    /// result in the transient status line.
    func applyCacheConfig(to directory: URL) {
        statusLine = "applying cache config…"
        Task { [weak self] in
            let result = CacheConfigApplier.applyConfig(to: directory)
            self?.statusLine = result.message
        }
    }

    /// Copy the bundled `tools/bazel` admission wrapper into `<dir>/tools/bazel`.
    func installWrapper(in directory: URL) {
        statusLine = "installing build wrapper…"
        Task { [weak self] in
            let result = CacheConfigApplier.installWrapper(in: directory)
            self?.statusLine = result.message
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
