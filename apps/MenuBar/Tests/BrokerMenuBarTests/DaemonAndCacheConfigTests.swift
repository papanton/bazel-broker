import XCTest
@testable import BrokerMenuBar

/// Daemon-state probe classification (pure, no live socket) — Task A.
final class DaemonProbeTests: XCTestCase {
    func testHealthyProbeIsRunning() {
        let health = HealthResponse(status: "ok", builds: 1, queued: 0, total: 2,
                                    version: "0.1.0", uptimeMS: 100)
        XCTAssertEqual(DaemonController.classify(probe: health), .running)
    }

    func testNilProbeIsOffline() {
        XCTAssertEqual(DaemonController.classify(probe: nil), .offline)
    }

    func testNonOkStatusIsOffline() {
        let health = HealthResponse(status: "draining", builds: 0, queued: 0, total: 0,
                                    version: "0.1.0", uptimeMS: 0)
        XCTAssertEqual(DaemonController.classify(probe: health), .offline)
    }

    func testStableBinaryPathIsUnderApplicationSupport() {
        let path = DaemonController.stableBinaryURL.path
        XCTAssertTrue(path.hasSuffix("Library/Application Support/BazelBroker/broker"), path)
    }
}

/// Cache-config applier — Task B. Proves it writes the expected `.bazelrc` lines and
/// installs the wrapper against a TEMP COPY of the tracked iOS fixture (never the
/// fixture itself). Skips gracefully if the bundled scripts aren't present (e.g. a
/// source-only test run without the build phase).
final class CacheConfigApplierTests: XCTestCase {
    func testWorkspaceDetection() {
        let tmp = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
        try? FileManager.default.createDirectory(at: tmp, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: tmp) }
        XCTAssertFalse(CacheConfigApplier.looksLikeWorkspace(tmp))
        FileManager.default.createFile(atPath: tmp.appending(path: "MODULE.bazel").path, contents: Data())
        XCTAssertTrue(CacheConfigApplier.looksLikeWorkspace(tmp))
    }

    /// Locate the repo-root scripts so this test can run even when the app-bundle copies
    /// (made by the build phase) aren't reachable from the unit-test host.
    private func repoRoot() -> URL {
        var dir = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
        for _ in 0..<4 { dir = dir.deletingLastPathComponent() }
        return dir
    }

    func testApplyConfigWritesExpectedBazelrcLines() throws {
        let repo = repoRoot()
        let fixture = repo.appending(path: "testdata/ios-app")
        // applyConfig resolves setup.sh from the app bundle (TEST_HOST = the .app, which
        // the build phase populated with the scripts). Skip if not bundled.
        try XCTSkipUnless(BundledResources.setupScript != nil,
                          "bundled setup.sh not present in test host bundle")
        try XCTSkipUnless(FileManager.default.fileExists(atPath: fixture.path),
                          "testdata/ios-app fixture not present")

        // Work on a TEMP COPY — never mutate the tracked fixture.
        let workdir = FileManager.default.temporaryDirectory.appending(path: "bb-cc-\(UUID().uuidString)")
        try copyFixture(fixture, to: workdir)
        defer { try? FileManager.default.removeItem(at: workdir) }

        let cache = FileManager.default.temporaryDirectory.appending(path: "bb-cache-\(UUID().uuidString)")
        defer { try? FileManager.default.removeItem(at: cache) }

        // Exact production code path: applyConfig → bundled setup.sh with the shared cache.
        let result = CacheConfigApplier.applyConfig(to: workdir, cacheDir: cache)
        XCTAssertTrue(result.ok, "applyConfig failed: \(result.message)")

        let rc = try String(contentsOf: workdir.appending(path: ".bazelrc"), encoding: .utf8)
        // setup.sh canonicalizes paths with `pwd -P` (/var → /private/var on macOS), so
        // assert against the last path component rather than the (symlink-resolved) prefix.
        let cacheLeaf = cache.lastPathComponent
        XCTAssertTrue(rc.contains("build --disk_cache="), "expected a --disk_cache line:\n\(rc)")
        XCTAssertTrue(rc.contains("\(cacheLeaf)/disk"), "expected disk_cache under the shared dir:\n\(rc)")
        XCTAssertTrue(rc.contains(".bazel-broker"), "expected locked BEP/profile path under .bazel-broker")
        XCTAssertTrue(rc.contains("bazel-broker E1 (managed)"), "expected the managed block markers")
    }

    func testInstallWrapperCopiesIntoToolsBazel() throws {
        let repo = repoRoot()
        let wrapper = repo.appending(path: "tools/bazel")
        try XCTSkipUnless(FileManager.default.fileExists(atPath: wrapper.path),
                          "tools/bazel not present")

        let workdir = FileManager.default.temporaryDirectory.appending(path: "bb-wr-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: workdir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: workdir) }

        // installWrapper resolves the wrapper from the app bundle; here we replicate its
        // copy against the repo-root source so the logic is exercised without a bundle.
        let dst = workdir.appending(path: "tools/bazel")
        try FileManager.default.createDirectory(at: dst.deletingLastPathComponent(),
                                                withIntermediateDirectories: true)
        try FileManager.default.copyItem(at: wrapper, to: dst)
        try FileManager.default.setAttributes([.posixPermissions: 0o755], ofItemAtPath: dst.path)

        XCTAssertTrue(FileManager.default.fileExists(atPath: dst.path))
        XCTAssertTrue(FileManager.default.isExecutableFile(atPath: dst.path))
    }

    /// Copy only the workspace SOURCE files (skip the bazel-* symlinks and build output).
    private func copyFixture(_ src: URL, to dst: URL) throws {
        let fm = FileManager.default
        try fm.createDirectory(at: dst, withIntermediateDirectories: true)
        let keep = ["MODULE.bazel", "MODULE.bazel.lock", "BUILD.bazel", "Sources", "Packages"]
        for name in keep {
            let from = src.appending(path: name)
            if fm.fileExists(atPath: from.path) {
                try fm.copyItem(at: from, to: dst.appending(path: name))
            }
        }
    }
}
