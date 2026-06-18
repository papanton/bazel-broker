import Foundation
import XCTest
@testable import BrokerMenuBar

/// Shared fixture access for the test suite. Resolves the canonical
/// `testdata/api/*.json` (the byte-checked contract) relative to this source file —
/// `.../apps/MenuBar/Tests/BrokerMenuBarTests/<file>.swift` → repo root is 5 up —
/// and falls back to the bundle copy under `Fixtures/` if the repo isn't on disk.
enum Fixtures {
    static func data(_ name: String) throws -> Data {
        var dir = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
        for _ in 0..<4 { dir = dir.deletingLastPathComponent() }
        let canonical = dir.appending(path: "testdata/api/\(name)")
        if FileManager.default.fileExists(atPath: canonical.path) {
            return try Data(contentsOf: canonical)
        }
        if let url = Bundle(for: BundleToken.self)
            .url(forResource: (name as NSString).deletingPathExtension, withExtension: "json") {
            return try Data(contentsOf: url)
        }
        let vendored = URL(fileURLWithPath: #filePath)
            .deletingLastPathComponent().appending(path: "Fixtures/\(name)")
        return try Data(contentsOf: vendored)
    }

    static func builds() throws -> [Build] {
        try BrokerDecoder.shared.decode(BuildsResponse.self, from: data("builds.json")).builds
    }

    private final class BundleToken {}
}
