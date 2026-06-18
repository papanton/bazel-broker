import Foundation

/// The lifecycle state of the broker daemon, surfaced in the menu header.
enum DaemonState: Equatable {
    case running          // /healthz reachable
    case starting         // bootstrapped, polling /healthz until ready
    case offline          // not reachable and not (yet) started by us
    case failed(String)   // start attempt errored

    var label: String {
        switch self {
        case .running:        return "broker: running"
        case .starting:       return "broker: starting…"
        case .offline:        return "broker: offline"
        case .failed:         return "broker: error"
        }
    }
}

/// The outcome of a `launchctl`/`install.sh` action, with captured output for surfacing.
struct DaemonActionResult: Equatable {
    let ok: Bool
    let message: String
}

/// Owns the broker daemon's lifecycle from the app, WITHOUT making it a child process:
/// the daemon is installed as a per-user LaunchAgent (via the bundled `install.sh`) so it
/// persists independently of the app and OUTLIVES it (architecture §4: the daemon MUST
/// outlive any UI). The app only ensures it is running and offers start/restart/stop.
///
/// The bundled broker binary is copied to a STABLE location
/// (`~/Library/Application Support/BazelBroker/broker`) before install, because the .app
/// bundle path is volatile (DerivedData, re-signing) and a LaunchAgent must point at a
/// fixed path that survives the app being moved or rebuilt.
struct DaemonController {
    static let label = "com.bazelbroker.broker"

    /// Stable home for the broker binary the LaunchAgent points at.
    static var stableBinaryURL: URL {
        FileManager.default
            .homeDirectoryForCurrentUser
            .appending(path: "Library/Application Support/BazelBroker/broker")
    }

    // MARK: Probe (pure, testable)

    /// Classifies a `/healthz` probe into a `DaemonState`. Pure so the probe logic is
    /// unit-testable without a live socket: a decoded health body with status "ok" means
    /// running; anything else (no response, non-2xx, unparseable) means offline.
    static func classify(probe: HealthResponse?) -> DaemonState {
        probe?.status == "ok" ? .running : .offline
    }

    /// One-shot reachability probe against `GET /healthz` (the only auth-exempt route).
    /// Returns the decoded health body, or nil if unreachable / non-2xx / unparseable.
    static func probe(config: BrokerConfig,
                      session: URLSession = URLSession(configuration: .ephemeral),
                      timeout: TimeInterval = 1.5) async -> HealthResponse? {
        var req = URLRequest(url: BrokerEndpoints.http(config, "/healthz"))
        req.timeoutInterval = timeout
        guard
            let (data, resp) = try? await session.data(for: req),
            let http = resp as? HTTPURLResponse, (200...299).contains(http.statusCode),
            let health = try? BrokerDecoder.shared.decode(HealthResponse.self, from: data)
        else { return nil }
        return health
    }

    // MARK: Install / start (LaunchAgent — persists independent of the app)

    /// Copy the bundled broker binary to the stable location, then bootstrap it as a
    /// per-user LaunchAgent via the bundled `install.sh install <binary>`. Idempotent:
    /// `install.sh` boots out any existing agent before bootstrapping.
    static func startAgent() -> DaemonActionResult {
        guard let bundled = BundledResources.brokerBinary else {
            return DaemonActionResult(ok: false, message: "bundled broker binary not found in app Resources")
        }
        guard let installer = BundledResources.installScript else {
            return DaemonActionResult(ok: false, message: "bundled install.sh not found in app Resources")
        }
        do {
            try installStableBinary(from: bundled)
        } catch {
            return DaemonActionResult(ok: false, message: "copy broker: \(error.localizedDescription)")
        }
        // install.sh resolves the plist relative to its OWN dir, so it picks up the
        // bundled plist sitting next to it in Resources/. `install.sh install` is an
        // idempotent re-bootstrap (bootout + bootstrap), so it doubles as "restart".
        return ProcessRunner.run(installer.path, ["install", stableBinaryURL.path])
    }

    /// Stop = `install.sh uninstall` (bootout + remove the plist). The daemon stays
    /// stopped until the user starts it again.
    static func stopAgent() -> DaemonActionResult {
        guard let installer = BundledResources.installScript else {
            return DaemonActionResult(ok: false, message: "bundled install.sh not found in app Resources")
        }
        return ProcessRunner.run(installer.path, ["uninstall"])
    }

    /// Poll `/healthz` until the daemon answers or the timeout elapses, reusing one
    /// session across the whole wait rather than spinning up one per attempt.
    static func waitUntilReady(config: BrokerConfig, timeout: TimeInterval = 10) async -> Bool {
        let session = URLSession(configuration: .ephemeral)
        let deadline = Date().addingTimeInterval(timeout)
        while Date() < deadline {
            if await probe(config: config, session: session) != nil { return true }
            try? await Task.sleep(for: .milliseconds(400))
        }
        return false
    }

    // MARK: Helpers

    private static func installStableBinary(from bundled: URL) throws {
        let fm = FileManager.default
        try fm.createDirectory(at: stableBinaryURL.deletingLastPathComponent(),
                               withIntermediateDirectories: true)
        if fm.fileExists(atPath: stableBinaryURL.path) {
            try fm.removeItem(at: stableBinaryURL)
        }
        try fm.copyItem(at: bundled, to: stableBinaryURL)
        try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: stableBinaryURL.path)
    }
}
