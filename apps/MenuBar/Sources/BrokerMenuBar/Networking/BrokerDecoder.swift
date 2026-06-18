import Foundation

/// The single shared decoder for every broker payload. The wire format is RFC3339
/// UTC (`time.RFC3339`, no fractional seconds — `internal/api/api.go FormatTime`),
/// which `.iso8601` parses. Defined once so the contract test and the live client
/// decode identically.
enum BrokerDecoder {
    /// The decoder is stateless and its configuration never changes, so a single
    /// shared instance is reused across the client and every (re)connect.
    static let shared: JSONDecoder = {
        let d = JSONDecoder()
        d.dateDecodingStrategy = .iso8601
        return d
    }()
}
