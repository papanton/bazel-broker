import Foundation

/// Applies the E1 shared-cache config to a user-chosen Bazel workspace by running the
/// bundled `cache-config/setup.sh`, and optionally installs the `tools/bazel` admission
/// wrapper. Thin Swift around the committed scripts — it does NOT reimplement them.
enum CacheConfigApplier {
    /// Shared default disk-cache root. `setup.sh` reads `BAZEL_BROKER_CACHE`; we also
    /// export `BROKER_DISK_CACHE` for parity with callers that expect that name.
    static var defaultCacheDir: URL {
        FileManager.default.homeDirectoryForCurrentUser.appending(path: ".cache/bazel-broker")
    }

    /// Heuristic workspace check (matches setup.sh's own gate): a Bazel workspace root
    /// has a MODULE.bazel / WORKSPACE / WORKSPACE.bazel, and typically BUILD files.
    static func looksLikeWorkspace(_ dir: URL) -> Bool {
        let fm = FileManager.default
        let markers = ["MODULE.bazel", "WORKSPACE", "WORKSPACE.bazel", "BUILD", "BUILD.bazel"]
        return markers.contains { fm.fileExists(atPath: dir.appending(path: $0).path) }
    }

    /// Run `setup.sh <dir>` with the shared cache dir. Validates the directory first and
    /// prefixes a warning (rather than hard-failing in the app) when it does not look like
    /// a workspace; the script itself still enforces the hard requirement.
    static func applyConfig(to directory: URL,
                            cacheDir: URL? = nil) -> DaemonActionResult {
        guard let setup = BundledResources.setupScript else {
            return DaemonActionResult(ok: false, message: "bundled setup.sh not found in app Resources")
        }
        let warning = looksLikeWorkspace(directory)
            ? ""
            : "warning: \(directory.lastPathComponent) has no MODULE.bazel/WORKSPACE/BUILD — "
        let cache = (cacheDir ?? defaultCacheDir).path
        let result = ProcessRunner.run(setup.path, [directory.path], env: [
            "BAZEL_BROKER_CACHE": cache,   // the name setup.sh actually reads
            "BROKER_DISK_CACHE": cache,    // parity with the task's documented name
        ])
        if result.ok {
            return DaemonActionResult(ok: true, message: "cache config applied to \(directory.lastPathComponent)")
        }
        return DaemonActionResult(ok: false, message: "\(warning)setup failed: \(result.message)")
    }

    /// Copy the bundled `tools/bazel` wrapper into `<dir>/tools/bazel` (chmod +x), so the
    /// chosen workspace also gets block-before-build admission. Clearly optional.
    static func installWrapper(in directory: URL) -> DaemonActionResult {
        guard let wrapper = BundledResources.bazelWrapper else {
            return DaemonActionResult(ok: false, message: "bundled bazel wrapper not found in app Resources")
        }
        let fm = FileManager.default
        let toolsDir = directory.appending(path: "tools")
        let dst = toolsDir.appending(path: "bazel")
        do {
            try fm.createDirectory(at: toolsDir, withIntermediateDirectories: true)
            if fm.fileExists(atPath: dst.path) { try fm.removeItem(at: dst) }
            try fm.copyItem(at: wrapper, to: dst)
            try fm.setAttributes([.posixPermissions: 0o755], ofItemAtPath: dst.path)
        } catch {
            return DaemonActionResult(ok: false, message: "install wrapper: \(error.localizedDescription)")
        }
        return DaemonActionResult(ok: true, message: "build wrapper installed in \(directory.lastPathComponent)/tools")
    }
}
