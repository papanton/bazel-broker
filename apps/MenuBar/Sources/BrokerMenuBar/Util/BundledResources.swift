import Foundation

/// Locates the helper binaries/scripts that the build phase copied into the app's
/// `Resources/` (see `scripts/bundle-resources.sh`): the broker daemon, the LaunchAgent
/// install script + plist template, and the cache-config setup script + bazel wrapper.
///
/// These resources have no file extension Xcode would index as a typed resource, so we
/// resolve them by name against `Bundle.main.resourceURL` directly.
enum BundledResources {
    /// The broker daemon binary bundled in Resources/.
    static let brokerName = "broker"
    /// The LaunchAgent install/uninstall script (`install.sh install <binary>`).
    /// (The plist template `com.bazelbroker.broker.plist` is also bundled, but it is
    /// consumed by install.sh — which resolves it relative to its own dir — not Swift.)
    static let installScriptName = "install.sh"
    /// The E1 cache-config writer (`setup.sh <workspace>`).
    static let setupScriptName = "setup.sh"
    /// The committed-in-each-worktree admission wrapper.
    static let bazelWrapperName = "bazel"

    /// URL of a bundled resource by name, or nil if missing (e.g. a stale build).
    static func url(_ name: String) -> URL? {
        guard let base = Bundle.main.resourceURL else { return nil }
        let url = base.appending(path: name)
        return FileManager.default.fileExists(atPath: url.path) ? url : nil
    }

    static var brokerBinary: URL? { url(brokerName) }
    static var installScript: URL? { url(installScriptName) }
    static var setupScript: URL? { url(setupScriptName) }
    static var bazelWrapper: URL? { url(bazelWrapperName) }
}
