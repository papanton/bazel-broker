import Foundation

/// Builds the broker's loopback URLs from `BrokerConfig`. Host comes from the
/// (advisory) config `host`, port from `port`; the daemon binds loopback only.
enum BrokerEndpoints {
    static func http(_ config: BrokerConfig, _ path: String) -> URL {
        url(scheme: "http", config: config, path: path)
    }

    static func ws(_ config: BrokerConfig, _ path: String) -> URL {
        url(scheme: "ws", config: config, path: path)
    }

    private static func url(scheme: String, config: BrokerConfig, path: String) -> URL {
        var c = URLComponents()
        c.scheme = scheme
        c.host = config.host
        c.port = config.port
        c.path = path.hasPrefix("/") ? path : "/" + path
        guard let u = c.url else {
            // host/port come from a validated config; this is unreachable in practice.
            return URL(string: "\(scheme)://127.0.0.1:\(config.port)\(path)")!
        }
        return u
    }
}
