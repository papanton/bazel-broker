import XCTest
@testable import BrokerMenuBar

/// The hard correctness gate (E8 §5.1): every golden fixture in `testdata/api/*.json`
/// must decode through the real Codable models with zero failures. If this passes,
/// the Swift client and the Go daemon agree on the wire contract byte-for-byte.
///
/// Fixtures are resolved from the canonical `testdata/api/` (walked up from the source
/// file at compile time) and fall back to the bundle copy under `Fixtures/`.
final class ContractDecodeTests: XCTestCase {
    private let decoder = BrokerDecoder.shared

    private func fixtureData(_ name: String) throws -> Data { try Fixtures.data(name) }

    // MARK: builds.json — GET /builds wrapper

    func testDecodeBuildsResponse() throws {
        let data = try fixtureData("builds.json")
        let resp = try decoder.decode(BuildsResponse.self, from: data)
        XCTAssertEqual(resp.builds.count, 2)

        let running = resp.builds[0]
        XCTAssertEqual(running.invocationID, "a1b2")
        XCTAssertEqual(running.worktree, "/wt/feature-a")
        XCTAssertEqual(running.worktreeName, "feature-a")
        XCTAssertEqual(running.displayName, "feature-a")
        XCTAssertEqual(running.targets, ["//app:App"])
        XCTAssertEqual(running.pid, 4242)
        XCTAssertEqual(running.state, .running)          // NOT "building"
        XCTAssertEqual(running.exitCode, 0)
        XCTAssertEqual(running.source, .registered)
        XCTAssertEqual(running.elapsedMS, 3120)
        XCTAssertNil(running.endTime)                    // omitted until terminal
        XCTAssertNil(running.cacheHitRatio)              // absent until E4
        XCTAssertNil(running.profileURL)
        XCTAssertTrue(running.isActive)

        // RFC3339 date decoded correctly.
        let comps = Calendar(identifier: .gregorian)
            .dateComponents(in: TimeZone(identifier: "UTC")!, from: running.startTime)
        XCTAssertEqual(comps.year, 2026)
        XCTAssertEqual(comps.hour, 9)
        XCTAssertEqual(comps.minute, 41)
        XCTAssertEqual(comps.second, 12)

        let finished = resp.builds[1]
        XCTAssertEqual(finished.invocationID, "c3d4")
        XCTAssertEqual(finished.state, .finished)
        XCTAssertEqual(finished.targets, ["//lib:lib", "//app:App"])
        XCTAssertNotNil(finished.endTime)                // terminal → present
        XCTAssertEqual(finished.cacheHitRatio ?? -1, 0.87, accuracy: 0.0001)
        XCTAssertEqual(finished.profileURL, "http://127.0.0.1:8765/builds/c3d4/profile")
        XCTAssertFalse(finished.isActive)
        XCTAssertEqual(finished.elapsedMS, 228000)
    }

    // MARK: build.json — single-build wrapper

    func testDecodeBuildResponse() throws {
        let data = try fixtureData("build.json")
        let resp = try decoder.decode(BuildResponse.self, from: data)
        XCTAssertEqual(resp.build.invocationID, "a1b2")
        XCTAssertEqual(resp.build.state, .running)
        XCTAssertEqual(resp.build.displayName, "feature-a")
    }

    // MARK: healthz.json

    func testDecodeHealth() throws {
        let data = try fixtureData("healthz.json")
        let health = try decoder.decode(HealthResponse.self, from: data)
        XCTAssertEqual(health.status, "ok")
        XCTAssertEqual(health.builds, 1)
        XCTAssertEqual(health.queued, 0)
        XCTAssertEqual(health.total, 2)
        XCTAssertEqual(health.version, "0.1.0")
        XCTAssertEqual(health.uptimeMS, 1423)
    }

    // MARK: event_snapshot.json — WS snapshot event

    func testDecodeSnapshotEvent() throws {
        let data = try fixtureData("event_snapshot.json")
        let event = try decoder.decode(Event.self, from: data)
        XCTAssertEqual(event.type, .snapshot)
        XCTAssertEqual(event.seq, 0)
        XCTAssertNil(event.build)
        XCTAssertEqual(event.builds?.count, 2)
        XCTAssertEqual(event.builds?.first?.invocationID, "a1b2")
        XCTAssertEqual(event.builds?.first?.state, .running)
    }

    // MARK: event_build.json — WS build (upsert) event

    func testDecodeBuildEvent() throws {
        let data = try fixtureData("event_build.json")
        let event = try decoder.decode(Event.self, from: data)
        XCTAssertEqual(event.type, .build)
        XCTAssertEqual(event.seq, 1)
        XCTAssertNil(event.builds)
        XCTAssertEqual(event.build?.invocationID, "a1b2")
        XCTAssertEqual(event.build?.state, .running)
    }

    // MARK: every fixture decodes without throwing (sweep)

    func testAllFixturesDecodeWithoutThrowing() throws {
        XCTAssertNoThrow(try decoder.decode(BuildsResponse.self, from: try fixtureData("builds.json")))
        XCTAssertNoThrow(try decoder.decode(BuildResponse.self, from: try fixtureData("build.json")))
        XCTAssertNoThrow(try decoder.decode(HealthResponse.self, from: try fixtureData("healthz.json")))
        XCTAssertNoThrow(try decoder.decode(Event.self, from: try fixtureData("event_snapshot.json")))
        XCTAssertNoThrow(try decoder.decode(Event.self, from: try fixtureData("event_build.json")))
    }
}

/// Forward-compatibility: a newer daemon emitting an unknown enum value must map to
/// `.unknown` rather than throwing.
final class ForwardCompatTests: XCTestCase {
    private let decoder = BrokerDecoder.shared

    func testUnknownStateMapsToUnknown() throws {
        let json = """
        {"invocation_id":"x","worktree":"/wt/x","targets":[],"pid":1,
         "state":"teleporting","start_time":"2026-06-17T09:41:12Z","exit_code":0,
         "source":"registered","elapsed_ms":0}
        """.data(using: .utf8)!
        let build = try decoder.decode(Build.self, from: json)
        XCTAssertEqual(build.state, .unknown)
    }

    func testUnknownSourceMapsToUnknown() throws {
        let json = """
        {"invocation_id":"x","worktree":"/wt/x","targets":[],"pid":1,
         "state":"running","start_time":"2026-06-17T09:41:12Z","exit_code":0,
         "source":"satellite","elapsed_ms":0}
        """.data(using: .utf8)!
        let build = try decoder.decode(Build.self, from: json)
        XCTAssertEqual(build.source, .unknown)
    }

    func testReservedMetricsEventMapsToUnknown() throws {
        let json = """
        {"type":"metrics","seq":5,"ts":"2026-06-17T09:41:12Z"}
        """.data(using: .utf8)!
        let event = try decoder.decode(Event.self, from: json)
        XCTAssertEqual(event.type, .unknown)
    }

    func testConfigDefaultsHostWhenAbsent() throws {
        let json = #"{"port":8765,"token":"deadbeef"}"#.data(using: .utf8)!
        let cfg = try JSONDecoder().decode(BrokerConfig.self, from: json)
        XCTAssertEqual(cfg.host, "127.0.0.1")
        XCTAssertEqual(cfg.port, 8765)
        XCTAssertEqual(cfg.token, "deadbeef")
    }

    func testConfigIgnoresUnknownKeys() throws {
        let json = #"{"host":"127.0.0.1","port":9000,"token":"t","disk_cache":"/x","max_concurrency":2,"db_path":"/d","log_path":"/l","profile_open":"perfetto"}"#.data(using: .utf8)!
        let cfg = try JSONDecoder().decode(BrokerConfig.self, from: json)
        XCTAssertEqual(cfg.port, 9000)
        XCTAssertEqual(cfg.token, "t")
    }
}

/// Small checks on the display-logic helpers (the only non-trivial view-side code).
final class FormatterTests: XCTestCase {
    func testElapsedFormatting() {
        XCTAssertEqual(ElapsedFormatter.format(millis: 3120), "3s")
        XCTAssertEqual(ElapsedFormatter.format(millis: 48000), "48s")
        XCTAssertEqual(ElapsedFormatter.format(millis: 192000), "3m12s")
        XCTAssertEqual(ElapsedFormatter.format(millis: 228000), "3m48s")
        XCTAssertEqual(ElapsedFormatter.format(millis: 3_720_000), "1h02m")
    }

    func testDisplayNameFallsBackToLastPathComponent() {
        let build = Build(
            invocationID: "x", worktree: "/wt/feature-z", worktreeName: nil,
            targets: [], pid: 1, state: .running,
            startTime: Date(), endTime: nil, exitCode: 0, source: .registered,
            elapsedMS: 0, cacheHitRatio: nil, profileURL: nil
        )
        XCTAssertEqual(build.displayName, "feature-z")
    }
}
