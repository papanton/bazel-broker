import Foundation

/// Reads the daemon-owned `config.json` (written by E2 — `internal/config/config.go`).
/// The app only needs `host`, `port`, and `token`; every other key (disk_cache,
/// max_concurrency, db_path, log_path, profile_open) is present in the file but
/// ignored — Codable drops unknown keys. `host` is advisory (the daemon always binds
/// loopback); it defaults to 127.0.0.1 when absent.
struct BrokerConfig: Codable {
    let host: String
    let port: Int
    let token: String

    enum CodingKeys: String, CodingKey {
        case host, port, token
    }

    init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        self.host = (try c.decodeIfPresent(String.self, forKey: .host)) ?? "127.0.0.1"
        self.port = try c.decode(Int.self, forKey: .port)
        self.token = try c.decode(String.self, forKey: .token)
    }

    init(host: String = "127.0.0.1", port: Int, token: String) {
        self.host = host
        self.port = port
        self.token = token
    }
}
