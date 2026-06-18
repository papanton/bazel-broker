import Foundation

/// One WS frame from `/events` (mirrors `api.Event`). The envelope has EXACTLY two
/// live types: `snapshot` (full list once on connect, carries `builds`) and `build`
/// (a single build to upsert by `invocation_id`, carries `build`). There is no
/// build_added/updated/finished/removed and no `heartbeat` JSON event — keepalives
/// arrive as WS ping frames handled by `URLSessionWebSocketTask`. `metrics`/`alert`
/// are reserved for E4 and decode to `.unknown` here.
struct Event: Codable {
    let type: EventType
    let seq: UInt64
    let build: Build?
    let builds: [Build]?
    let ts: Date

    enum CodingKeys: String, CodingKey {
        case type, seq, build, builds, ts
    }
}

/// WS event discriminator. Unrecognized types (E4's reserved `metrics`/`alert`, or a
/// future type) map to `.unknown` so the reducer can ignore them without throwing.
enum EventType: String, Codable, UnknownFallbackDecodable {
    case snapshot, build, unknown
    static var unknownCase: EventType { .unknown }
}
