import Foundation

/// `GET /builds` returns `{"builds":[…]}` (mirrors `api.BuildsResponse`), NOT a bare
/// array. Decode the wrapper, then read `.builds`.
struct BuildsResponse: Codable {
    let builds: [Build]
}

/// `GET /builds/{id}` (and `/register`, `/deregister`) wrap a single build as
/// `{"build":{…}}` (mirrors `api.BuildResponse`).
struct BuildResponse: Codable {
    let build: Build
}

/// `GET /healthz` body (mirrors `api.HealthResponse`). Auth-exempt; used as a cheap
/// reachability probe.
struct HealthResponse: Codable {
    let status: String
    let builds: Int
    let queued: Int
    let total: Int
    let version: String
    let uptimeMS: Int64

    enum CodingKeys: String, CodingKey {
        case status, builds, queued, total, version
        case uptimeMS = "uptime_ms"
    }
}

/// `POST /builds/{id}/kill` response (mirrors `api.KillResult`). Filled by E3; until
/// then the route returns 501, which the client treats as "kill not available yet".
struct KillResult: Codable {
    let killed: Bool
    let invocationID: String
    let pid: Int
    let outcome: String
    let elapsedMS: Int64

    enum CodingKeys: String, CodingKey {
        case killed, pid, outcome
        case invocationID = "invocation_id"
        case elapsedMS = "elapsed_ms"
    }
}

/// Body of every non-2xx response (mirrors `api.ErrorResponse`).
struct ErrorResponse: Codable {
    let error: String
    let message: String?
    let epic: String?
}
