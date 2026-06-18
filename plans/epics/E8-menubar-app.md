# E8 — SwiftUI menu-bar app

> ⚠️ **Superseded where it conflicts with [`00-consolidated-review.md`](00-consolidated-review.md) + E2 §4 (frozen contract).** Conform before coding:
> - **P1:** the shared types mirror **`internal/api`** (not `internal/wire`) — fix the 4 refs; Codable field names match E2 §4.1 verbatim (`invocation_id`/`start_time`/`cache_hit_ratio`, state `running`, two WS types `snapshot`/`build`, no `heartbeat` JSON event).
> - E2 must ship **golden JSON fixtures** (`testdata/api/*.json`) for the Swift decode test (P6).
> - User decision: **D-stack-2** (TCP+token vs Unix socket) gates the client transport.

> Executable implementation plan for the native macOS menu-bar front-end of the Bazel Broker.
> A thin, logic-free VIEW over the broker daemon: glance + kill from the menu bar.

Status: **Draft v2 (conformed to E2 §4 frozen contract)** · Owner: Antonis · Last updated: 2026-06-17
Maps to architecture: §5 C5c, §9 tech stack, §10 GUI verification exception · Maps to milestone M5
Depends on: **E2** (broker API + token) · best with **E3** (kill / discovery) and **E4** (cache metrics)

> **Contract source of truth:** E2's `internal/wire/wire.go` and §4 ("FROZEN — consumed by
> E3/E4/E5/E6/E7/E8") define every JSON shape, route, enum value, and the WS event envelope.
> This epic's Codable models in §2.4 are now **mirrored from E2 §4 verbatim** (field names,
> RFC3339 timestamps, the `snapshot`/`build` two-event envelope, the `{"builds":[…]}` wrapper).
> Earlier drafts of this file *guessed* a divergent wire shape (a `build_added`/`build_updated`/
> `build_removed`/`metrics_updated` envelope, an `output_base` field, a `host` config key, bare
> `[Build]` responses, per-build `/builds/{id}/kill` routes); E2 §4.6 explicitly supersedes those
> guesses, and they have been corrected here. See the Staff Engineer Review at the end for the
> full diff and the remaining cross-epic items to escalate.

---

## 1. Goal & scope recap

Deliver the native "small Mac app" from C5c: a SwiftUI `MenuBarExtra` that sits in the
macOS menu bar and gives a glanceable, killable view of Bazel builds across worktrees,
driven entirely by the broker daemon.

**In scope (from 02-epics.md E8):**
- SwiftUI `MenuBarExtra` (macOS 13+ API; we target macOS 26 / arm64).
- Networking to the broker over `URLSession` (HTTP) + `URLSessionWebSocketTask` (live `/events`).
- Bearer token loaded from `~/.config/bazel-broker/config.json` (daemon-owned, written by E2).
- Codable models **mirrored from E2 §4** (`wire.Build`, `wire.BuildsResponse`, `wire.Event`,
  config) — NOT invented here. `MetricsSummary` tracks E4's `metrics` DDL columns until E4 freezes.
- Menu UI: header "N building, M queued"; per-build row (worktree, targets, elapsed,
  kill button); cache% badge; "open profile in Perfetto" action.
- Project structure (Xcode project — see §2.1 for the project-format decision) producing an
  ad-hoc-signed local `.app`.
- Verification exclusively via the `xcodebuildmcp-cli` skill (build / launch / screenshot /
  UI-automate), per architecture §10's GUI exception.

**Explicitly out of scope (keep logic in the daemon — architecture §5 split rationale):**
- No admission decisions, no kill-grace state machine, no metric computation, no cache GC in
  the app. The app **renders** state and **POSTs** intents (`kill`); the daemon owns all logic.
- No Homebrew cask packaging in this epic — `brew-cask` is **deferred** (02-epics.md E8:
  "brew-cask later"; 01-architecture decision: ships via Homebrew cask, ad-hoc signing for
  personal use, **NOT** the Mac App Store). This epic ships an ad-hoc-signed local `.app` only.
- No sandbox, no Mac App Store entitlements (architecture: "No App Store, no sandbox").
- No notifications / Dock presence / Settings window beyond a minimal "Quit" + "Reconnect".

**Done when (acceptance, verbatim from E8):**
> menu bar reflects live builds (verified via `xcodebuildmcp-cli` build/launch/screenshot);
> kill button stops a build; cache% matches `brokerctl`.

---

## 2. Design & implementation details

### 2.0 Architectural stance

The app is a **pure projection** of broker state. There is exactly one source of truth (the
daemon's registry + SQLite store). The app holds an in-memory snapshot, refreshed by:
1. an initial `GET /builds` (and `GET /metrics`) on launch / reconnect, then
2. incremental WS events from `/events` applied to that snapshot.

No persistence, no caching to disk, no business logic. If the daemon is down, the app shows a
"disconnected" state and retries. This keeps the verifiable surface tiny (architecture §10).

### 2.1 Project structure — **OPEN DECISION D-E8-1** (see §6)

The epic deliverable is "`.app` + Xcode/SwiftPM project". Two viable shapes; I am **not**
unilaterally picking — surfaced as D-E8-1. The plan below is written so either works, with a
**recommended default** to keep sequencing concrete.

- **Option A (recommended default): Xcode project** (`.xcodeproj`) with a single macOS App
  target. Cleanest path for `MenuBarExtra` (no Storyboard, `LSUIElement`/agent activation
  policy set in Info.plist), signing settings live in the target, and `xcodebuildmcp-cli`
  drives `xcodebuild` against a `.xcodeproj`/scheme directly. Chosen as the default because the
  verification skill and ad-hoc signing both key off an Xcode scheme.
- **Option B: SwiftPM executable** (`Package.swift`, `.executableTarget`) packaged into a
  `.app` bundle via a small script (or `xcodebuild` on a generated project). Lighter to read in
  git, but bundling a SwiftUI menu-bar `LSUIElement` app + Info.plist + ad-hoc signing by hand
  is fiddly and less directly drivable by the skill.

> The file/code layout below assumes **Option A**. If D-E8-1 resolves to B, the same Swift
> sources move under `Sources/BrokerMenuBar/` and a packaging script replaces the Xcode target;
> no source code changes.

### 2.2 File layout (Option A — Xcode project)

```
app/                                  # lives in the broker repo, sibling to cmd/ (architecture repo layout)
  BrokerMenuBar.xcodeproj/
  BrokerMenuBar/
    BrokerMenuBarApp.swift            # @main App + MenuBarExtra scene
    Info.plist                        # LSUIElement = YES (agent app, no Dock icon)
    BrokerMenuBar.entitlements        # NO sandbox; (only if needed) network client; ad-hoc
    Models/
      Build.swift                     # Codable: Build (mirrors wire.Build), BuildState
      BuildsResponse.swift            # Codable: { "builds": [Build] } (mirrors wire.BuildsResponse)
      MetricsSummary.swift            # Codable: MetricsSummary (tracks E4 metrics DDL; PROVISIONAL)
      Events.swift                    # Codable: WS Event envelope (mirrors wire.Event: snapshot|build)
      BrokerConfig.swift              # Codable: config.json { port, token, … } — reads token only
    Networking/
      BrokerClient.swift              # actor: URLSession HTTP + WS, decode, reconnect
      BrokerEndpoints.swift           # URL builder: host HARD-CODED 127.0.0.1 + config.port + paths
      TokenLoader.swift               # reads ~/.config/bazel-broker/config.json
    State/
      BrokerStore.swift              # @MainActor @Observable: snapshot the views render
    Views/
      MenuRootView.swift             # header + list + footer; the MenuBarExtra content
      MenuHeaderView.swift           # "N building, M queued" + cache% badge + conn status
      BuildRowView.swift             # one build: worktree, targets, elapsed, kill button
      DisconnectedView.swift         # shown when WS/HTTP unreachable
    Util/
      ElapsedFormatter.swift         # Date math → "3m12s" (display-only)
      PerfettoOpener.swift           # NSWorkspace.open(profile deep-link URL)
  Makefile.app                       # `make -f Makefile.app build|launch|sign` (wraps xcodebuild)
  CLAUDE.md                          # "how to run & verify" recipe (architecture §10)
```

### 2.3 The MenuBarExtra scene

```swift
// BrokerMenuBarApp.swift
import SwiftUI

@main
struct BrokerMenuBarApp: App {
    // Single owned store; created once for the app lifetime.
    @State private var store = BrokerStore()

    var body: some Scene {
        MenuBarExtra {
            MenuRootView()
                .environment(store)
                .frame(width: 360)        // .menuBarExtraStyle(.window) gives a real view, not an NSMenu
        } label: {
            // Glanceable label: count of building, tinted on failure/disconnect.
            MenuBarLabel(summary: store.summary, connection: store.connection)
        }
        .menuBarExtraStyle(.window)        // REQUIRED: .menu style can't host buttons/live rows well
    }
}

// Tiny label view — purely derived from store; no logic.
struct MenuBarLabel: View {
    let summary: BuildSummary            // building/queued counts (computed in store from snapshot)
    let connection: ConnectionState
    var body: some View {
        switch connection {
        case .connected:
            Image(systemName: summary.building > 0 ? "hammer.fill" : "hammer")
            Text("\(summary.building)")   // e.g. menu bar shows "🔨 3"
        case .connecting, .disconnected:
            Image(systemName: "hammer.badge.exclamationmark")  // muted/error glyph
        }
    }
}
```

Notes:
- `.menuBarExtraStyle(.window)` is required so the dropdown is a real SwiftUI view hierarchy
  (buttons, hover, live-updating rows). The default `.menu` style renders an `NSMenu` and is
  too restrictive for kill buttons + a metrics badge.
- `LSUIElement = YES` in Info.plist → no Dock icon, no main window: a pure menu-bar agent.
- The store starts/stops its connection in `.task`/`onAppear` of `MenuRootView` (see §2.7) so
  networking is tied to the menu being constructed, and the WS can idle when never opened
  (open decision D-E8-3, §6 — connect-eagerly vs connect-on-open).

### 2.4 Codable models — mirrored from E2 §4 (the authoritative, FROZEN contract)

These are a **field-for-field mirror of `internal/wire/wire.go`** (E2 §4.1), not a proposal.
Every field name, the snake_case `CodingKeys`, the RFC3339-string timestamps, the `{"builds":…}`
response wrapper, and the two-event WS envelope (`snapshot` | `build`) come straight from E2's
frozen contract. The app must not add or rename fields. (The one genuinely unfrozen model is
`MetricsSummary`, owned by E4 — see the explicit caveat below it.)

```swift
// Models/Build.swift  — mirrors wire.Build (E2 §4.1)
struct Build: Codable, Identifiable, Hashable {
    let invocationID: String       // wire: invocation_id (Bazel BuildStarted.uuid). Primary key.
    let worktree: String           // absolute worktree path; UI shows last path component
    let targets: [String]          // e.g. ["//app:App"]
    let pid: Int                    // wire.Build.pid is a NON-optional Int (0 if unknown), per E2
    let state: BuildState
    let startTime: Date            // wire: start_time, RFC3339 UTC → decoded via .iso8601
    let endTime: Date?             // wire: end_time, omitted until terminal → optional
    let exitCode: Int               // wire: exit_code, meaningful only in terminal states
    let source: BuildSource        // wire: "registered" | "discovered"
    let elapsedMS: Int64           // wire: elapsed_ms, computed server-side (do NOT recompute; §2.8)

    var id: String { invocationID }   // Identifiable on invocation_id

    enum CodingKeys: String, CodingKey {
        case invocationID = "invocation_id"
        case worktree, targets, pid, state, source
        case startTime  = "start_time"
        case endTime    = "end_time"
        case exitCode   = "exit_code"
        case elapsedMS  = "elapsed_ms"
    }
}

// NOTE: there is NO `output_base` on the wire. In E2, bepPath/profile are reserved *internal*
// domain fields explicitly NOT serialized (E2 §2.2). The Perfetto profile location comes from
// E4's /metrics (profile path), NOT from Build. The earlier `outputBase` field was removed.

enum BuildState: String, Codable {
    case queued, running, finished, failed, killed, unknown
    // E2 wire values are EXACTLY these (note: "running", NOT "building"; "unknown" is a real
    // E2 state for a discovered process whose outcome was never observed). The header's "N
    // building" count maps to state == .running (see BuildSummary in §2.7).
    // Forward-compat: a custom decoder maps any unrecognized raw value to .unknown (below).
}

enum BuildSource: String, Codable {
    case registered, discovered, unknown   // wire: "registered" | "discovered"; .unknown = fallback
}
```

```swift
// Models/BuildsResponse.swift  — mirrors wire.BuildsResponse (E2 §4.1)
// GET /builds returns {"builds":[…]}, NOT a bare array. The WS `snapshot` event also carries
// `builds`. Decode the wrapper, then read `.builds`.
struct BuildsResponse: Codable { let builds: [Build] }
```

```swift
// Models/Events.swift  — mirrors wire.Event (E2 §4.1). The WS envelope has EXACTLY two types:
// "snapshot" (full list, once on connect) and "build" (one created/updated/terminated build).
// There is NO build_added/build_updated/build_removed/metrics_updated/heartbeat. Removal and
// state changes are conveyed as a single `build` event whose `state` field tells the story
// (e.g. state == .killed/.finished). Keepalive is a WS-protocol ping (E2 §2.6: 30s ping), not
// an application frame, so the app never decodes a heartbeat envelope.
struct Event: Codable {
    let type: EventType
    let seq: UInt64                // monotonic per connection (E2 §4.1); usable for gap detection
    let build: Build?             // set when type == .build
    let builds: [Build]?          // set when type == .snapshot
    let ts: Date                   // RFC3339 UTC emit time → .iso8601

    enum CodingKeys: String, CodingKey { case type, seq, build, builds, ts }
}

enum EventType: String, Codable {
    case snapshot          // full [Build] list, sent once on connect (E2's resync contract)
    case build             // a single build created / updated / terminated
    case unknown           // forward-compat fallback for a future E2 event type
}
```

```swift
// Models/BrokerConfig.swift  — mirrors E2's config.json writer (E2 §2.5).
// E2 writes {port, token, disk_cache, max_concurrency, db_path, log_path}. There is NO `host`
// key: the base is ALWAYS 127.0.0.1 (E2 binds loopback only; §2.6). The app reads `port` +
// `token` and ignores the rest; extra keys decode harmlessly (Codable ignores unknown keys).
struct BrokerConfig: Codable {
    let port: Int
    let token: String
    // disk_cache / max_concurrency / db_path / log_path are present in the file but unused by
    // the app; not declared here. Host is hard-coded to 127.0.0.1 in BrokerEndpoints (§4.3).

    enum CodingKeys: String, CodingKey { case port, token }
}
```

```swift
// Models/MetricsSummary.swift  — PROVISIONAL: owned by E4, NOT frozen (D-E8-5).
// `GET /metrics` is a reserved route in E2 (returns 501 until E4 lands; E2 §4.2). E4 will
// freeze the JSON. Until then, these field names track E2's `metrics` DDL columns (E2 §2.4:
// actions_total, actions_cached, cache_hit_ratio, wall_ms) — the best available signal — plus
// a profile path for the Perfetto action. EVERY field is optional so a partial/early payload
// decodes. DO NOT build T6 against these names until E4 ratifies them (escalated as D-E8-5).
struct MetricsSummary: Codable, Hashable {
    let invocationID: String?      // wire: invocation_id; nil = aggregate across active builds
    let cacheHitRatio: Double?     // wire: cache_hit_ratio (E2 DDL name — note _ratio, not _rate); 0.0–1.0
    let actionsTotal: Int?         // wire: actions_total
    let actionsCached: Int?        // wire: actions_cached
    let wallMS: Int64?             // wire: wall_ms (E2 DDL name; NOT duration_seconds)
    let profilePath: String?       // E4-defined: path to --profile .gz for the Perfetto link

    enum CodingKeys: String, CodingKey {
        case invocationID = "invocation_id"
        case cacheHitRatio = "cache_hit_ratio"
        case actionsTotal = "actions_total"
        case actionsCached = "actions_cached"
        case wallMS = "wall_ms"
        case profilePath = "profile_path"
    }
}
```

**Forward-compat decoding:** `BuildState`, `BuildSource`, and `EventType` each implement
`init(from:)` that maps any unrecognized raw value to the `.unknown` case instead of throwing,
so a newer daemon adding a state/source/event never crashes this thin client. The shared
`JSONDecoder` sets `dateDecodingStrategy = .iso8601` (E2 emits RFC3339 UTC with a `Z`, which
`.iso8601` parses; E2 §4 timestamps carry no fractional seconds, so the default `.iso8601`
formatter is sufficient — if E4 ever emits fractional seconds, switch to a custom
`.iso8601withFractionalSeconds` strategy). This is the only defensive code the thin client needs.

### 2.5 Token + config loading

```swift
// Networking/TokenLoader.swift
enum TokenLoaderError: Error { case missing, unreadable, malformed }

struct TokenLoader {
    // Resolve the config path the SAME way E2 §2.5 does, in priority order:
    //   1. $BAZEL_BROKER_CONFIG (explicit override)
    //   2. $XDG_CONFIG_HOME/bazel-broker/config.json
    //   3. ~/.config/bazel-broker/config.json
    // Mismatching this resolution would miss a relocated config (D-E8-4).
    static func configURL() -> URL {
        let env = ProcessInfo.processInfo.environment
        if let p = env["BAZEL_BROKER_CONFIG"], !p.isEmpty {
            return URL(fileURLWithPath: (p as NSString).expandingTildeInPath)
        }
        if let xdg = env["XDG_CONFIG_HOME"], !xdg.isEmpty {
            return URL(fileURLWithPath: xdg).appending(path: "bazel-broker/config.json")
        }
        return FileManager.default.homeDirectoryForCurrentUser
            .appending(path: ".config/bazel-broker/config.json")
    }

    static func load() throws -> BrokerConfig {
        let url = configURL()
        guard FileManager.default.fileExists(atPath: url.path) else { throw TokenLoaderError.missing }
        guard let data = try? Data(contentsOf: url) else { throw TokenLoaderError.unreadable }
        do { return try JSONDecoder().decode(BrokerConfig.self, from: data) }
        catch { throw TokenLoaderError.malformed }
    }
}
```

- Read **once at connect time** and on explicit "Reconnect"; the daemon may rotate the token or
  port on restart, so re-read on every (re)connection attempt rather than caching forever.
- The app runs **non-sandboxed** specifically so it can read this daemon-owned file under
  `~/.config` (a sandboxed app could not; reinforces "no sandbox / no MAS"). Surfaced as a risk
  in §6 (file-ownership / readability — see D-E8-4).
- If the file is missing/malformed, the app shows `DisconnectedView` with "broker not
  configured — is the daemon running?" and a Reconnect button. It never blocks or crashes.

### 2.6 BrokerClient actor (URLSession + WebSocket)

```swift
// Networking/BrokerClient.swift
actor BrokerClient {
    private let session: URLSession
    private var config: BrokerConfig
    private var wsTask: URLSessionWebSocketTask?
    private let decoder: JSONDecoder

    init(config: BrokerConfig) {
        self.config = config
        self.session = URLSession(configuration: .ephemeral)   // no disk cache; loopback only
        self.decoder = {
            let d = JSONDecoder(); d.dateDecodingStrategy = .iso8601; return d
        }()
    }

    // --- HTTP: initial snapshot + intents (paths/verbs/bodies are E2 §4.2 verbatim) ---
    // GET /builds returns the WRAPPER {"builds":[…]} (wire.BuildsResponse), not a bare array.
    func fetchBuilds() async throws -> [Build] {
        try await get("/builds", as: BuildsResponse.self).builds
    }
    // GET /metrics?invocation_id=… (E2 §4.2). Reserved → 501 until E4 lands: the store layer
    // treats a 501 as "no metrics yet" (nil), NOT as a connection failure. Shape is PROVISIONAL.
    func fetchMetrics(invocationID: String) async throws -> MetricsSummary? {
        try await getOptional("/metrics?invocation_id=\(invocationID)", as: MetricsSummary.self)
    }
    // POST /kill with body {"invocation_id":"…"} (E2 §4.2). NOT /builds/{id}/kill.
    // Reserved → 501 until E3 lands; store treats 501 as "kill not available yet".
    func kill(invocationID: String) async throws {
        var req = request(path: "/kill"); req.httpMethod = "POST"
        req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        req.httpBody = try JSONEncoder().encode(["invocation_id": invocationID])
        let (_, resp) = try await session.data(for: req)
        try Self.checkStatus(resp)                              // 2xx else throw (incl. 501→typed)
    }

    private func get<T: Decodable>(_ path: String, as: T.Type) async throws -> T {
        let (data, resp) = try await session.data(for: request(path: path))
        try Self.checkStatus(resp)
        return try decoder.decode(T.self, from: data)
    }
    // Returns nil on 501 (reserved route, owning epic not landed) instead of throwing.
    private func getOptional<T: Decodable>(_ path: String, as: T.Type) async throws -> T? {
        let (data, resp) = try await session.data(for: request(path: path))
        if (resp as? HTTPURLResponse)?.statusCode == 501 { return nil }
        try Self.checkStatus(resp)
        return try decoder.decode(T.self, from: data)
    }

    private func request(path: String) -> URLRequest {
        var r = URLRequest(url: BrokerEndpoints.http(config, path))
        r.setValue("Bearer \(config.token)", forHTTPHeaderField: "Authorization")
        return r
    }

    // --- WebSocket: live event stream, yielded as an AsyncStream of wire.Event envelopes ---
    func events() -> AsyncThrowingStream<Event, Error> {
        AsyncThrowingStream { continuation in
            Task { await self.runEventLoop(continuation) }
        }
    }

    private func runEventLoop(_ cont: AsyncThrowingStream<Event, Error>.Continuation) async {
        var req = URLRequest(url: BrokerEndpoints.ws(config, "/events"))
        // E2 §2.6/§4: WS /events requires `Authorization: Bearer <token>` on the UPGRADE request.
        // URLSessionWebSocketTask DOES send custom headers on the HTTP upgrade (unlike the
        // browser WebSocket API, which cannot — that's E7's problem, not ours). However,
        // `Authorization` is on URLSession's *reserved* header set and CAN be silently dropped
        // depending on configuration. Mitigations, in order:
        //   1. Set it on the per-request URLRequest here (works in practice on macOS 26 for WS).
        //   2. If E2 ever rejects the upgrade as 401, fall back to E2's documented WS auth shim:
        //      pass the token via `Sec-WebSocket-Protocol` (E2 §2.6 mentions this shim for E7;
        //      E8 can reuse it) using AcceptOptions/`addValue` for that header.
        //   3. Last resort (loopback only): a one-shot `GET /healthz`→token-in-query handshake.
        // T4's checkpoint MUST assert the WS upgrade authenticates (not just that frames arrive);
        // this is the single riskiest interop point in the epic (see §6 risk "WS upgrade auth").
        req.setValue("Bearer \(config.token)", forHTTPHeaderField: "Authorization")
        let task = session.webSocketTask(with: req)
        self.wsTask = task
        task.resume()
        do {
            while !Task.isCancelled {
                let msg = try await task.receive()
                switch msg {
                case .string(let s):
                    if let env = try? decoder.decode(Event.self, from: Data(s.utf8)) {
                        cont.yield(env)
                    }                                          // unknown/garbage frame: skip, don't die
                case .data(let d):
                    if let env = try? decoder.decode(Event.self, from: d) { cont.yield(env) }
                @unknown default: break
                }
            }
            cont.finish()
        } catch {
            cont.finish(throwing: error)                       // store layer schedules a backoff reconnect
        }
    }

    func close() { wsTask?.cancel(with: .goingAway, reason: nil); wsTask = nil }
}
```

Notes:
- The actor isolates all networking state; the bearer token is applied to **both** the HTTP
  requests and the WS upgrade request (E2 authenticates the WS handshake — see the inline WS-auth
  caveat above; this is the riskiest interop point and gets an explicit T4 assertion).
- E2's WS ping/pong (30s, §2.6) is handled at the protocol layer by `URLSessionWebSocketTask`;
  there is no application-level `heartbeat` frame to decode. The app does not send pings itself.
- `.ephemeral` session: no on-disk URL cache or cookies; loopback-only traffic.
- WS reconnection/backoff is owned by the **store** (§2.7), not the client — when `events()`
  finishes with an error, the store waits (exponential backoff, capped) and re-creates the
  stream after re-reading config. (D-E8-3 governs *when* to first connect.)

### 2.7 BrokerStore (the @MainActor observable snapshot)

```swift
// State/BrokerStore.swift
@MainActor @Observable
final class BrokerStore {
    private(set) var builds: [Build] = []
    private(set) var metricsByBuild: [String: MetricsSummary] = [:]
    private(set) var aggregateCacheHitRate: Double? = nil
    private(set) var connection: ConnectionState = .connecting

    private var client: BrokerClient?
    private var eventTask: Task<Void, Never>?
    private var backoff = Backoff(min: 0.5, max: 30)          // exponential, capped

    var summary: BuildSummary {                               // derived, no stored logic
        // E2's wire state for an active build is .running (NOT .building); the menu-bar label's
        // "N building" is a UI word for state == .running.
        let building = builds.filter { $0.state == .running }.count
        let queued   = builds.filter { $0.state == .queued }.count
        return BuildSummary(building: building, queued: queued)
    }

    func start() { reconnect() }                              // called from MenuRootView.task

    func reconnect() {
        eventTask?.cancel()
        eventTask = Task { await self.connectLoop() }
    }

    private func connectLoop() async {
        while !Task.isCancelled {
            do {
                let cfg = try TokenLoader.load()
                let client = BrokerClient(config: cfg); self.client = client
                connection = .connecting
                // 1) initial snapshot via HTTP. /metrics is E4-owned (501 until E4); a nil
                //    result is NOT a connection error — the snapshot still renders without cache%.
                let initial = try await client.fetchBuilds()
                apply(snapshot: initial)
                // metrics fetched per build, best-effort (501/absent → skipped, no throw):
                for b in initial {
                    if let m = try? await client.fetchMetrics(invocationID: b.invocationID) {
                        apply(metrics: m)
                    }
                }
                connection = .connected
                backoff.reset()
                // 2) live deltas via WS until it errors/closes. The first frame is the
                //    `snapshot` event (E2 resync contract); apply(event:) handles snapshot|build.
                for try await ev in await client.events() { apply(event: ev) }
            } catch {
                connection = .disconnected
                try? await Task.sleep(for: .seconds(backoff.next()))   // backoff then loop
            }
        }
    }

    func kill(_ id: String) {                                 // fire-and-forget intent; daemon owns the kill
        Task { try? await client?.kill(invocationID: id) }   // UI updates when the `build` event (state=.killed) arrives
    }

    // Pure reducers, no I/O:
    //   apply(snapshot:)        ← initial GET /builds (and the WS `snapshot` event)
    //   apply(metrics:)         ← per-build MetricsSummary, keyed by invocation_id
    //   apply(event:)           ← WS Event: .snapshot replaces `builds`; .build upserts/terminates
    //                             one build by invocation_id (a terminal state row is dropped or
    //                             shown greyed per §2.8). .unknown events are ignored.
}

enum ConnectionState { case connecting, connected, disconnected }
struct BuildSummary { let building: Int; let queued: Int }
```

Key properties:
- `@Observable` + `@MainActor` → SwiftUI re-renders automatically when `builds`/`metrics`/
  `connection` change; views never call the network directly.
- **The kill button does NOT optimistically mutate local state.** It POSTs the intent (`POST
  /kill`) and waits for the authoritative `build` event (E2's only update event) carrying the
  build now in a terminal `state` (`.killed`). This is the literal expression of "all logic in
  the daemon" and makes the kill independently verifiable (the row leaves because the daemon said
  so, not because the app guessed).
- Backoff lives here so the WS reconnect policy is one small, testable place.

### 2.8 Menu views

```swift
// Views/MenuRootView.swift
struct MenuRootView: View {
    @Environment(BrokerStore.self) private var store
    var body: some View {
        VStack(spacing: 0) {
            MenuHeaderView()
            Divider()
            switch store.connection {
            case .connected:
                if store.builds.isEmpty { Text("No active builds").foregroundStyle(.secondary).padding() }
                else {
                    ScrollView { LazyVStack(spacing: 0) {
                        ForEach(store.builds) { build in
                            BuildRowView(build: build,
                                         metrics: store.metricsByBuild[build.id])
                            Divider()
                        }
                    } }.frame(maxHeight: 400)
                }
            case .connecting, .disconnected:
                DisconnectedView()
            }
            Divider()
            HStack {
                Button("Reconnect") { store.reconnect() }
                Spacer()
                Button("Quit") { NSApplication.shared.terminate(nil) }
            }.padding(8)
        }
        .task { store.start() }                               // connect tied to menu lifetime
    }
}
```

```swift
// Views/BuildRowView.swift
struct BuildRowView: View {
    let build: Build
    let metrics: MetricsSummary?
    @Environment(BrokerStore.self) private var store
    @State private var now = Date()                          // ticked by a TimelineView for live elapsed

    var body: some View {
        HStack(alignment: .top) {
            VStack(alignment: .leading, spacing: 2) {
                Text(build.worktree.lastPathComponent).font(.headline)    // "feature-a"
                Text(build.targets.joined(separator: " ")).font(.caption).foregroundStyle(.secondary)
                HStack(spacing: 8) {
                    // Live elapsed seeds from the server's elapsed_ms (authoritative, skew-free)
                    // and ticks locally; for terminal builds use end_time. See §2.8 note.
                    Text(ElapsedFormatter.string(startTime: build.startTime,
                                                 endTime: build.endTime,
                                                 serverElapsedMS: build.elapsedMS,
                                                 now: now))   // "3m12s"
                    if let r = metrics?.cacheHitRatio {
                        CacheBadge(ratio: r)                 // e.g. "cache 92%"
                    }
                    StateBadge(state: build.state)
                }.font(.caption2)
            }
            Spacer()
            if metrics?.profilePath != nil {
                Button { PerfettoOpener.open(metrics!) } label: { Image(systemName: "chart.bar.xaxis") }
                    .help("Open profile in Perfetto")
            }
            Button(role: .destructive) { store.kill(build.invocationID) } label: { Image(systemName: "stop.circle") }
                .help("Kill build")
                .accessibilityIdentifier("kill-\(build.invocationID)")   // for §5 verification
                .disabled(build.state != .running && build.state != .queued)
        }
        .padding(8)
    }
}
```

- Live elapsed time is driven by a `TimelineView(.periodic(...))` (or a 1 Hz timer feeding
  `now`) so rows tick without server traffic — pure display, the daemon need not push per-second
  updates. **Seed from the server's `elapsed_ms`** (E2 computes it server-side from a monotonic
  wall clock; E2 §6 "Clock skew / elapsed") rather than `now - start_time`, so the displayed
  elapsed is authoritative at fetch time and only the local *increment* is client-side. For
  terminal builds, freeze at `end_time - start_time`. This sidesteps the clock-skew non-issue
  cleanly and matches `brokerctl`'s number exactly.
- `accessibilityIdentifier`s are added to the kill button, cache badge, and header counts so
  `xcodebuildmcp-cli` UI-automation can find and click them deterministically (§5).

### 2.9 Perfetto action

```swift
// Util/PerfettoOpener.swift
enum PerfettoOpener {
    static func open(_ m: MetricsSummary) {
        // Per architecture C4/E4, the broker serves the profile over localhost and exposes a
        // deep-link. The app just opens that URL; it does NOT parse/serve the profile itself.
        guard let path = m.profilePath else { return }
        let url = BrokerEndpoints.perfettoDeepLink(forProfile: path)   // built from broker base URL
        NSWorkspace.shared.open(url)
    }
}
```

The exact deep-link shape (does E4 serve `ui.perfetto.dev/#!/?url=http://127.0.0.1:PORT/...`,
or open a local Perfetto, or just reveal the `.gz`?) is **owned by E4** and is surfaced as
D-E8-5 in §6 — the app must consume whatever E4 exposes, not define it.

---

## 3. Sequencing (ordered, checkpointed task list)

Each task is independently buildable/verifiable. Tasks are ordered so the app is launchable and
screenshot-able as early as possible, then made live, then made interactive.

> **Gating dependency:** E2 must expose `GET /healthz`, `GET /builds`, `/events` (WS), and write
> `~/.config/bazel-broker/config.json` before T3 can connect to a *real* daemon. Until then,
> T0–T2 use a hand-run broker stub or the fake-bazel fixtures from E0. T5 (kill) needs E3;
> T6 (cache%) needs E4. Tasks are arranged so the app is fully demoable against whatever subset
> of E2/E3/E4 exists.

- **T0 — Project skeleton + ad-hoc build.** Create the Xcode project (Option A; or resolve
  D-E8-1 first), `LSUIElement=YES`, empty `MenuBarExtra` showing a static label + "Quit".
  Configure ad-hoc signing (`CODE_SIGN_IDENTITY=-`, no team). *Checkpoint:* `xcodebuildmcp-cli`
  builds the scheme and launches a menu-bar item; screenshot shows the glyph. No daemon needed.

- **T1 — Codable models + decoder config.** Add `Build`, `BuildsResponse`, `Event`,
  `BrokerConfig` (and provisional `MetricsSummary`) with `CodingKeys`, `.iso8601` dates, and
  unknown-enum fallback (`.unknown` for `BuildState`/`BuildSource`/`EventType`). *Checkpoint:*
  a unit test decodes **E2's golden fixtures verbatim** (§4.4) — `builds.json`,
  `event_snapshot.json`, `event_build.json` — and asserts the wrapper, `running` state, RFC3339
  dates, and unknown-enum tolerance. (This is the one place unit tests are worth it; see §5.)

- **T2 — TokenLoader + BrokerEndpoints.** Read `~/.config/bazel-broker/config.json`; build HTTP
  & WS URLs. *Checkpoint:* with a hand-written config.json, a debug action logs the resolved
  base URL; with the file absent, returns the typed error (no crash).

- **T3 — BrokerClient HTTP + initial snapshot wired to the store.** `fetchBuilds()`/
  `fetchMetrics()`, `BrokerStore.apply(snapshot:)`, render real rows. *Checkpoint:* against a
  running E2 daemon (or stub) with one Register'd build, the menu lists it with worktree +
  targets + elapsed. Verified via screenshot.

- **T4 — WebSocket live updates + reconnect/backoff.** `events()` stream, `apply(event:)`
  reducers, `connectLoop` backoff, `DisconnectedView`. *Checkpoint:* start daemon → app
  connects; add/finish a fake build → row appears/disappears live; kill the daemon → app shows
  Disconnected; restart daemon → app auto-reconnects. Screenshots at each phase.

- **T5 — Kill button (needs E3).** Wire `store.kill(id)` → `POST /builds/{id}/kill`; row updates
  only on the daemon's event. *Checkpoint:* launch a long fake-bazel build, click kill in the
  app (or via UI-automate), assert the build exits with the cancel code (E0 fake-bazel SIGINT
  contract) and the row leaves the menu.

- **T6 — Cache% badge + header summary + Perfetto action (needs E4).** Render `cacheHitRatio`,
  "N building, M queued" header, and the Perfetto button. *Checkpoint:* cache% shown in the app
  equals `brokerctl ls --json`'s value for the same build; Perfetto button opens the E4 deep-link.

- **T7 — Polish + verify recipe.** Accessibility identifiers, empty/disconnected states, the
  `Makefile.app` targets and `CLAUDE.md` run/verify recipe. *Checkpoint:* the full `/verify`
  recipe in §5 runs green end-to-end via `xcodebuildmcp-cli`.

---

## 4. Interfaces & contracts

### 4.1 Broker endpoints consumed (HTTP/WS) — **taken verbatim from E2 §4.2 (FROZEN)**

| App call | Broker endpoint (E2 §4.2) | Auth | Notes |
|---|---|---|---|
| connect probe (optional) | `GET /healthz` | none | `HealthResponse` `{status,builds,queued,total,version,uptime_ms}` — lightweight reachability check; the only unauth route |
| initial build list | `GET /builds` | bearer | returns **`BuildsResponse` `{"builds":[…]}`** (NOT a bare array); decode the wrapper |
| per-build metrics | `GET /metrics?invocation_id=…` | bearer | **reserved → `501` until E4**; app treats 501 as "no metrics yet" |
| kill | `POST /kill` body `{"invocation_id":"…"}` | bearer | **reserved → `501` until E3**; fire-and-forget, 2xx = accepted |
| live feed | `WS /events` (HTTP upgrade) | bearer (header on upgrade) | first frame is a `snapshot` event, then incremental `build` events (§2.4) |

> **These are no longer guesses.** Earlier this table proposed `/builds/{id}/kill`,
> `/builds/{id}/metrics`, and a bare-array `/builds`; **all three were wrong** and are corrected
> above to E2's frozen §4.2. The remaining open item is the *metrics JSON body* (E4-owned), not
> the routes.

### 4.2 Auth

- Bearer token in the `Authorization: Bearer <token>` header on **every** HTTP request **and**
  on the WS upgrade request (E2 §4: every route except `/healthz` requires it; missing/wrong →
  `401 {"error":"unauthorized"}`). Loopback TCP + token is the recommended transport (D-stack-2,
  *still open at the E2 level* — see §6). Token + port come from the config file below.
- **WS upgrade auth is the riskiest interop point** (§2.6 inline note): `URLSessionWebSocketTask`
  sends custom headers on the upgrade, but `Authorization` is reserved and can be dropped. T4
  must assert the upgrade authenticates; fallback is E2's `Sec-WebSocket-Protocol` token shim.

### 4.3 Config token path (shared with E2)

- `~/.config/bazel-broker/config.json`, written/owned by the E2 daemon (`0600`, dir `0700`;
  E2 §2.5). **Actual shape E2 writes (E2 §2.5):**
  ```json
  { "port": 8765, "token": "<32-byte-hex>", "disk_cache": "…", "max_concurrency": 2,
    "db_path": "…", "log_path": "…" }
  ```
  **There is no `host` key** — E2 binds loopback only, so the app hard-codes `127.0.0.1`
  (`BrokerEndpoints`, §2.4 `BrokerConfig`). The app reads `port` + `token` and ignores the rest
  (unknown keys decode harmlessly). E2 also honors `$BAZEL_BROKER_CONFIG` / `$XDG_CONFIG_HOME`
  for the path (E2 §2.5); the app should resolve the path the same way (see D-E8-4).

### 4.4 JSON model contract (shared with E2/E4)

`Build`, `BuildsResponse`, and `Event` in §2.4 are **mirrors of E2's frozen `internal/wire`
structs** (E2 §4.1), not proposals — the Go and Swift definitions must stay byte-compatible.
To keep both sides honest and to feed T1's decode test, **E2 should commit golden JSON fixtures
from its own httptest/WS tests** (e.g. `testdata/api/builds.json`, `event_snapshot.json`,
`event_build.json`) — E2's §4.3 already contains concrete example payloads that can seed them.
This app decodes those exact fixtures in a unit test (T1). That fixture set is the executable
contract: if it decodes, app and daemon agree. The `metrics.json` fixture is **deferred to E4**
(D-E8-5), since `/metrics` is reserved until then.

> **Escalate (cross-epic handshake):** E8 cannot freeze its `MetricsSummary` or build T6 until
> **E4 publishes the `/metrics` JSON body + a golden fixture**. The `Build`/`Event` fixtures are
> E2's to publish and should land with E2 T7 (WS) — request them as an E2 deliverable.

### 4.5 WS event semantics consumed (E2 §4.1 — only TWO event types)

The E2 envelope is **`snapshot` | `build`** — there is no add/update/remove/metrics/heartbeat
event. The app's reducer:

- `snapshot` (first frame on connect, E2's resync contract) — carries the full `builds` array;
  **replace** the local snapshot wholesale. (The app's launch `GET /builds` is belt-and-suspenders;
  the WS `snapshot` alone would suffice, but the HTTP fetch lets the UI populate before the WS
  upgrade completes — D-E8-3.)
- `build` — carries one full `Build`; **upsert by `invocation_id`**. A terminal `state`
  (`finished`/`failed`/`killed`/`unknown`) is how a row "leaves" — there is no separate removal
  event; the reducer drops or greys terminal rows per the UI policy in §2.8.
- WS ping/pong keepalive (E2 §2.6, 30s) is handled by `URLSessionWebSocketTask` at the protocol
  layer — **never surfaces as an application frame to decode.**
- `seq` (monotonic per connection) lets the app detect a gap; on a gap or any error the store
  drops the WS and re-runs the snapshot path (§2.7), which is lossless given the `snapshot`-on-
  connect contract.

---

## 5. Testing & verification

Per architecture §10, the app is the **GUI exception**: it is verified via the
`xcodebuildmcp-cli` skill (build / launch / screenshot / UI-automate), and we deliberately keep
the verifiable surface tiny by putting all logic in the daemon. Unit testing is limited to the
one genuinely logic-bearing piece: JSON decoding (T1).

### 5.1 What gets unit-tested (small)
- **Codable round-trip / decode of golden fixtures** (§4.4): every model decodes the E2/E4
  golden JSON; unknown enum values and missing optionals do not throw. This is the only XCTest/
  Swift Testing target.

### 5.2 What gets verified via `xcodebuildmcp-cli` (the bulk)
Use the skill to: build the scheme (`macos build`), launch the `.app` (`macos launch`), and
**screenshot**. **Reality check (verified against the installed CLI):** the skill's first-class
macOS verbs are `macos build / build-and-run / launch / screenshot / stop`. Its rich
**`ui-automation` tools (`tap`/`snapshot-ui`/`wait-for-ui`) are iOS-Simulator-oriented** (AXe
HID, simulator elementRefs) and do **not** drive a macOS `NSStatusItem` menu-bar dropdown. So:

- **Build / launch / screenshot of the macOS app: fully supported by the skill.** This covers
  Done-when #1 and #3 as *visual* assertions.
- **Clicking the menu-bar item and its kill button is NOT a first-class skill capability on
  macOS.** Do not assume `tap` works against the status item. Instead, make the kill action
  **independently drivable without the GUI** so verification doesn't depend on automating an
  `NSStatusItem`:
  - The authoritative kill assertion is at the API layer: `POST /kill` (or `brokerctl kill <id>`)
    → fake-bazel exits with the E0 cancel code. That is the real "kill works" proof.
  - The *app's* role (POST on tap, row leaves on the daemon's `build` event) is asserted by a
    **launch-argument test hook**: the app accepts `-killOnLaunch <invocation_id>` (DEBUG-only)
    so the skill can `macos launch`-with-args to exercise the exact `store.kill` → POST → event
    path headlessly, then screenshot the now-empty row. This keeps the GUI verifiable without a
    menu-bar UI-automation driver the skill doesn't provide.
  - If true status-item clicking is later required, it needs an **AppleScript/`cliclick` System
    Events** path (outside `xcodebuildmcp-cli`) — flagged as a verification risk in §6, not a
    silent assumption.

1. **Menu reflects live builds** (Done-when #1): start the broker (E2) + register/launch fake
   builds (E0 `fake-bazel.sh`); `macos launch` the app; **screenshot** the opened panel (the app
   can open its `MenuBarExtra` panel on launch in a DEBUG verify mode); assert the row count and
   worktree labels match `brokerctl ls --json`.
2. **Kill button stops a build** (Done-when #2): launch a long `fake-bazel` build; drive the kill
   via the `-killOnLaunch` hook (or `POST /kill`/`brokerctl kill` for the API-level proof);
   assert (a) the fake-bazel process exits with the E0 cancel code, and (b) the row leaves on the
   next screenshot. The non-optimistic store (§2.7) guarantees the row leaves *because of the
   daemon's `build` event*, not a client guess — which is exactly what makes this verifiable.
3. **Cache% matches brokerctl** (Done-when #3, needs E4): after a build reports `BuildMetrics`,
   screenshot the row's cache badge and assert its percentage equals `brokerctl ls --json`'s
   `cache_hit_ratio` for that invocation (E2/E4 field name is `cache_hit_ratio`).
4. **Reconnect behavior:** kill the daemon → screenshot shows Disconnected; restart → screenshot
   shows builds again (no app restart).

### 5.3 `/verify` recipe (to live in CLAUDE.md)

```
# Preconditions: E2 broker built; E0 fake-bazel available; config.json present.
make -f app/Makefile.app build           # xcodebuild, ad-hoc sign
broker &                                  # start daemon (E2) — writes config.json
FAKE_BAZEL_DURATION=60 testdata/fake-bazel.sh --build_event_json_file=/tmp/b.json &  # a build to see/kill
# via xcodebuildmcp-cli skill (macOS verbs only — no simulator UI-automation against the status item):
#   1. xcodebuildmcp macos build   --project app/BrokerMenuBar.xcodeproj --scheme BrokerMenuBar
#   2. xcodebuildmcp macos launch  ...BrokerMenuBar.app  (DEBUG verify mode opens the panel)
#   3. xcodebuildmcp macos screenshot  -> assert >=1 row, worktree label correct
#   4. compare against:  brokerctl ls --json   (or curl GET /builds -> .builds[])
#   5. kill via the proof path:  curl -X POST /kill -d '{"invocation_id":"<id>"}'  (or brokerctl kill <id>)
#      and, for the APP path, relaunch with  -killOnLaunch <id>  (DEBUG hook) to exercise store.kill
#   6. assert fake-bazel exited with the E0 cancel code; screenshot shows the row gone
#   7. (with E4) screenshot cache badge == brokerctl/GET-/builds cache_hit_ratio
```

### 5.4 Acceptance criteria (from "Done when")
- [ ] Menu bar reflects live builds, verified via `xcodebuildmcp-cli` build/launch/screenshot.
- [ ] Kill button stops a build (fake-bazel exits with cancel code; row leaves menu).
- [ ] Cache% shown in the app matches `brokerctl` for the same invocation.
- [ ] App builds + launches ad-hoc-signed with no Developer ID / notarization / sandbox.

---

## 6. Risks, edge cases, open decisions

### Open decisions (surfaced, NOT resolved here)
- **D-E8-1 — Project format:** Xcode `.xcodeproj` (recommended, §2.1) vs SwiftPM executable
  bundled into a `.app`. Drives §2.2 layout and the `Makefile.app` packaging. *Recommend A.*
- **D-E8-2 — Wire contract with E2: now FROZEN and conformed (no longer open for `Build`/`Event`/
  config/routes).** E2 §4 froze the route table, the `wire.Build`/`wire.Event` JSON, the config
  keys, and the two-event WS envelope; this plan's §2.4/§4 are now mirrored to it. **The only
  remaining contract gap is E4's `/metrics` body** (reserved → 501 until E4) — folded into
  D-E8-5. **Escalate as a deliverable, not a decision:** ask E2 to ship golden JSON fixtures with
  its T7 (WS) so T1's decode test is the executable contract (§4.4). Do not start **T3** against a
  real daemon until E2 T6 (`/builds`) + T7 (`/events`) land; until then use a stub emitting E2's
  §4.3 example payloads.
- **D-E8-3 — Connect eagerly vs on-open.** Open the WS at app launch (always-fresh menu, steady
  background traffic) vs only when the menu is opened (lazier, but first open shows stale/empty
  for a beat). Affects §2.3/§2.7 `.task` placement. *Lean: connect eagerly but cheaply, since
  loopback + a small daemon is nearly free; revisit if it ever matters.*
- **D-E8-4 — config.json readability/ownership: LOW RISK, effectively confirmed by E2.** E2 §2.8
  installs a **per-user LaunchAgent** (`launchctl bootstrap gui/$(id -u)`, `~/Library/LaunchAgents`),
  explicitly "runs as the developer", and writes the config `0600` / dir `0700` (E2 §2.5). The
  non-sandboxed GUI app runs as that same login user, so the read succeeds. Two residual items to
  pin: (a) the app must resolve the path the **same way E2 does** — honor `$BAZEL_BROKER_CONFIG`
  and `$XDG_CONFIG_HOME` before falling back to `~/.config/...` (E2 §2.5), or a relocated config
  is missed; (b) confirm E2 never moves to a *system* launchd domain (it states it won't). *No
  blocking decision; just align the path-resolution logic with E2 §2.5.*
- **D-E8-5 — Perfetto deep-link shape** is owned by E4 (C4): does E4 serve the profile over
  localhost + hand back a `ui.perfetto.dev?url=...` link, reveal the `.gz`, or open a bundled
  Perfetto? The app just opens whatever URL E4 exposes (§2.9). Pin once E4 lands.

### Risks & edge cases
- **WS upgrade auth header may be stripped (HIGHEST interop risk).** E2 requires
  `Authorization: Bearer` on the `/events` upgrade. `URLSessionWebSocketTask` *does* send custom
  upgrade headers, but `Authorization` is on URLSession's reserved-header list and can be dropped
  in some configurations, yielding a silent `401` on the handshake. Mitigation: T4 explicitly
  asserts the upgrade authenticates (not just that frames flow); documented fallback is E2's
  `Sec-WebSocket-Protocol` token shim (E2 §2.6), which E8 reuses if the header path fails. This
  is the one thing most likely to cost a debugging session — surfaced inline in §2.6 and called
  out in §4.2.
- **macOS menu-bar UI is not driveable by `xcodebuildmcp-cli`'s UI-automation (verification gap).**
  The skill's `tap`/`snapshot-ui` tools target the iOS Simulator, not an `NSStatusItem`. The plan
  works around this by (a) proving kill at the API layer and (b) a DEBUG `-killOnLaunch` launch
  hook for the app path, plus build/launch/screenshot for the visual assertions (§5.2). True
  status-item clicking would need an out-of-band AppleScript/System Events path — flagged, not
  assumed.
- **WS reconnect storms.** A flapping daemon could cause tight reconnect loops; mitigated by the
  capped exponential backoff in `BrokerStore` (§2.7) and re-reading config each attempt.
- **App lifecycle vs daemon lifecycle.** The daemon outlives the app by design (architecture §5
  split rationale). The app must tolerate: daemon-not-running at launch (Disconnected state),
  daemon restart (token/port may change → re-read config on reconnect), and the app being quit
  with builds in flight (no effect — daemon keeps controlling). The app never owns build state.
- **Token rotation on daemon restart.** Because config is re-read on each (re)connect (§2.5), a
  rotated token/port is picked up automatically; a cached token would silently 401. Covered.
- **Unknown enum / new fields from a newer daemon.** Forward-compatible decoding (§2.4) prevents
  crashes when E2/E4 add states or event types. The thin client degrades gracefully (ignores).
- **Clock skew on elapsed.** Elapsed is **seeded from the server's `elapsed_ms`** (E2 computes it
  from a monotonic wall clock; E2 §6) and only *ticked* locally for live builds; terminal builds
  freeze at `end_time - start_time`. On a single Mac the clocks coincide anyway (single-Mac scope,
  N4), but seeding from `elapsed_ms` makes the displayed value match `brokerctl` exactly. Non-issue.
- **Non-sandboxed / ad-hoc signing, no MAS.** Confirmed by 01-architecture (Homebrew cask, ad-hoc
  for personal use, **not** MAS) and E8 ("ad-hoc signed local `.app`; brew-cask later"). Ad-hoc
  signature (`-`) means no notarization; first launch may need a Gatekeeper allow if ever moved
  off-machine, but for personal/local use it's fine. **brew-cask packaging is explicitly deferred
  out of this epic.** Risk: an ad-hoc app reading `~/.config` is only acceptable because it's
  non-sandboxed and personal-use; a future cask-distributed build for *others* would need
  Developer ID + notarization (architecture), which is a separate, later effort.
- **`MenuBarExtra` style limits.** `.menu` style can't host the kill buttons/badges we need;
  `.window` style (§2.3) is required and is the assumed minimum. Low risk on macOS 26.

---

## 7. Effort & internal ordering

Small epic by design (logic lives in the daemon). Rough sizing (one engineer, assuming E2 is up):

| Task | Effort | Blocks on |
|---|---|---|
| T0 project skeleton + ad-hoc build + first screenshot | ~0.5 day | D-E8-1 |
| T1 Codable models + decode test | ~0.5 day | D-E8-2 fixtures (can start with proposed shapes) |
| T2 TokenLoader + endpoints | ~0.25 day | E2 config.json shape (D-E8-2/D-E8-4) |
| T3 HTTP snapshot → store → rows | ~0.5 day | E2 `GET /builds` |
| T4 WebSocket live + reconnect/backoff | ~1 day | E2 `/events` |
| T5 kill button | ~0.25 day | **E3** |
| T6 cache% + header + Perfetto | ~0.5 day | **E4** (+ D-E8-5) |
| T7 polish + Makefile + CLAUDE.md verify recipe | ~0.5 day | — |

**Total: ~3.5–4 days** of app work, fanning out behind E2/E3/E4.

**Internal ordering rationale:**
1. T0 first so there is a launchable, screenshot-able artifact immediately (de-risks the
   `xcodebuildmcp-cli` + ad-hoc-signing path before any networking).
2. T1–T2 (pure, daemon-independent) can proceed in parallel with E2 finishing, using the
   proposed contract + golden fixtures.
3. T3→T4 turn it live (snapshot before stream, so the WS layer always has a baseline).
4. T5 (kill) and T6 (cache%) gate on E3 and E4 respectively and are last — they complete the
   two remaining "Done when" criteria. They are independent of each other and can be done in
   either order as E3/E4 land.
5. T7 packages the verify recipe so the whole epic is `/verify`-able headlessly.

**Critical path:** E2 (`/builds` + `/events` + config) → T3/T4. The kill and cache criteria are
gated by E3 and E4 and should be scheduled to follow those epics; do not promise the full
"Done when" set before E3 and E4 are merged.

---

## Staff Engineer Review

### (a) Verdict
**Approve with required changes — now applied.** The architecture is right: a non-sandboxed,
ad-hoc-signed `MenuBarExtra` that is a pure projection of broker state, with all logic in the
daemon and a non-optimistic kill, is exactly the thin client E8/C5c calls for, and it is the
correct shape for the `xcodebuildmcp-cli` GUI-verification exception. **But the original draft's
Codable models did not match E2's frozen §4 wire contract — they would not have decoded a single
real broker payload** (wrong state value, wrong event envelope, wrong response wrapper, wrong
routes, invented fields/keys). Those were correctness-blocking and are fixed in this revision.
The two genuinely hard dependencies (E2 wire fixtures; E4's `/metrics` body) are now framed as
escalations, not silently assumed.

### (b) Top findings
1. **Codable ↔ E2 mismatch (blocking, fixed).** Against E2 §4 (FROZEN), the draft was wrong on
   nearly every model: `BuildState` used `building` (E2 = `running`) and omitted `unknown`;
   `Build` invented `output_base` and made `pid` optional while dropping the real `end_time`,
   `exit_code`, `source`, `elapsed_ms`; the WS envelope invented a five-event
   `build_added/updated/removed/metrics_updated/heartbeat` model where E2 has exactly two events
   (`snapshot`, `build`); `/builds` was decoded as a bare `[Build]` where E2 returns
   `{"builds":[…]}`; `config.json` invented a `host` key E2 never writes. All conformed to
   `internal/wire` verbatim.
2. **Wrong routes (blocking, fixed).** Draft used `POST /builds/{id}/kill` and
   `GET /builds/{id}/metrics`; E2 §4.2 freezes `POST /kill` (body `{"invocation_id":…}`) and
   `GET /metrics?invocation_id=…`, both **reserved → 501** until E3/E4. The client now hits the
   real routes and treats 501 as "feature not landed", not as a connection failure.
3. **WS upgrade auth reachability (real risk, now explicit).** Yes, a SwiftUI app *can* reach a
   loopback bearer-token WS via `URLSessionWebSocketTask` — it sends custom upgrade headers
   (unlike the browser `WebSocket` API). The caveat: `Authorization` is a reserved URLSession
   header and can be dropped, silently 401-ing the handshake. Added an inline mitigation ladder
   (header → E2's `Sec-WebSocket-Protocol` shim → loopback last resort) and made T4 assert the
   upgrade authenticates.
4. **Token read from `~/.config` is sound for this app (confirmed).** Non-sandboxed + E2's
   per-user LaunchAgent (`gui/$(id -u)`, `0600`) running as the same login user ⇒ the read
   succeeds; a sandboxed/MAS app could not, which is precisely why architecture mandates no
   sandbox. The only fix needed was making the app resolve the config path the **same way E2
   does** (`$BAZEL_BROKER_CONFIG` / `$XDG_CONFIG_HOME` / `~/.config`), now in `TokenLoader`.
5. **`xcodebuildmcp-cli` verification was over-promised (fixed).** Verified against the installed
   CLI (v2.6.2): it has first-class macOS `build/launch/screenshot/stop`, but its `ui-automation`
   (`tap`/`snapshot-ui`) is iOS-Simulator-only and **cannot click an `NSStatusItem` menu-bar
   dropdown**. Re-grounded §5 on build/launch/screenshot + an API-level kill proof + a DEBUG
   `-killOnLaunch` launch hook, and flagged true status-item automation as out-of-band.
6. **Elapsed should seed from `elapsed_ms` (tightened).** E2 computes `elapsed_ms` server-side;
   seeding from it (and freezing terminal rows at `end_time - start_time`) makes the app's number
   match `brokerctl` exactly and is strictly better than `now - start_time`.

### (c) What I changed
- Rewrote **§2.4** to mirror `wire.Build`/`wire.BuildsResponse`/`wire.Event`/`BrokerConfig`
  field-for-field (correct `CodingKeys`, RFC3339/`.iso8601`, `running`+`unknown` states,
  `BuildSource`, two-event envelope, `seq`); removed `output_base`/`host`; marked `MetricsSummary`
  PROVISIONAL and tracked E2's `metrics` DDL names (`cache_hit_ratio`, `wall_ms`).
- Fixed **§2.6** client: `fetchBuilds` decodes the wrapper; `kill` → `POST /kill` with JSON body;
  `fetchMetrics(invocationID:)` + 501-tolerant `getOptional`; `events()` yields `Event`; added the
  WS-auth mitigation ladder and the "ping is protocol-level, not an app frame" note.
- Fixed **§2.7/§2.8** store+views: `summary` counts `.running`; metrics fetched per-build
  best-effort; reducer documented for `snapshot|build`; `kill(invocationID:)`; row uses
  `startTime`/`endTime`/`elapsedMS`/`cacheHitRatio`, adds the `kill-<id>` a11y id.
- Updated **§2.5** `TokenLoader` to E2's path-resolution order; **§3 T1** to decode E2 golden
  fixtures; **§4.1–4.5** to E2's frozen routes/auth/config/envelope; **§5** to the real
  `xcodebuildmcp-cli` capability set; **§6** decisions/risks (D-E8-2 now "frozen + conformed",
  D-E8-4 downgraded to low-risk, added WS-upgrade-auth and verification-tooling risks).
- Added a header banner pointing at E2 §4 as the contract source of truth and noting the
  superseded guesses. Preserved the 7-section structure throughout.

### (d) Decisions / risks to escalate (NOT resolved here)
- **E2 wire fixtures are a required E2 deliverable.** Ask E2 to ship `builds.json`,
  `event_snapshot.json`, `event_build.json` golden fixtures with its T7 — they are E8's T1
  executable contract. (Owner sign-off; E2-scoped.)
- **E4 `/metrics` JSON body is unfrozen (D-E8-5).** `MetricsSummary` is provisional; do not build
  **T6** until E4 publishes the body + a `metrics.json` fixture. Field names currently track E2's
  `metrics` DDL as the best signal.
- **D-stack-2 (TCP+token vs Unix socket) is still open at the E2/project level** and gates E7/E8
  per E2 §6. This plan assumes the recommended TCP+token; if it flips to a Unix socket,
  `URLSessionWebSocketTask`/`URLSession` cannot speak it directly and E8's transport needs rework.
  *Escalate for owner sign-off before T3.*
- **D-E8-1 (Xcode `.xcodeproj` vs SwiftPM), D-E8-3 (connect eager vs on-open), D-E8-5 (Perfetto
  deep-link shape)** remain open by design — framing improved, not decided.
- **Verification of the menu-bar UI itself** has no first-class skill path (finding #5); the
  API-level + launch-hook proofs are the pragmatic answer, but if stakeholder wants literal
  "click the menu-bar kill button" coverage, that's a separate out-of-band tooling decision.
