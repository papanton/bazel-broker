import Foundation

enum TokenLoaderError: Error, Equatable {
    case missing
    case unreadable
    case malformed
}

/// Resolves and loads the daemon's `config.json`, matching E2's path resolution
/// (`internal/config/config.go`): `$BAZEL_BROKER_CONFIG` overrides everything, else
/// `$XDG_CONFIG_HOME/bazel-broker/config.json`, else `~/.config/bazel-broker/config.json`.
/// Re-read on every (re)connect so a rotated token/port is picked up automatically.
enum TokenLoader {
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
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw TokenLoaderError.missing
        }
        guard let data = try? Data(contentsOf: url) else {
            throw TokenLoaderError.unreadable
        }
        do {
            return try JSONDecoder().decode(BrokerConfig.self, from: data)
        } catch {
            throw TokenLoaderError.malformed
        }
    }
}
