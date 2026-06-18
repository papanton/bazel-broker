import Foundation

/// Forward-compat decoding for the wire enums: a newer daemon adding a state,
/// source, or event type must not crash this thin client. Any `String`-backed enum
/// with an `.unknown` case conforms and gets an `init(from:)` that maps unrecognized
/// raw values to `.unknown` instead of throwing — the single source of that policy.
protocol UnknownFallbackDecodable: RawRepresentable, Decodable where RawValue == String {
    static var unknownCase: Self { get }
}

extension UnknownFallbackDecodable {
    init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = Self(rawValue: raw) ?? Self.unknownCase
    }
}
