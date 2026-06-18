import Foundation

/// `Build` is a field-for-field mirror of the broker's frozen `api.Build` DTO
/// (`internal/api/api.go`, E2 §4.1). The byte-checked golden serializations live in
/// `testdata/api/*.json`; `BrokerMenuBarTests` decodes them through this exact type.
///
/// Field freezes that matter: `invocation_id` (not `id`), `start_time` (not
/// `started_at`), state value `running` (not `building`), `cache_hit_ratio` is a
/// nullable 0..1 float, and `profile_url` is a fully-formed URL the app just `open`s
/// (E4 populates it). `worktree_name`, `end_time`, `cache_hit_ratio`, and
/// `profile_url` are `omitempty` on the wire, hence optional here. `pid`,
/// `exit_code`, and `elapsed_ms` are always present (non-optional on the wire).
struct Build: Codable, Identifiable, Hashable {
    let invocationID: String
    let worktree: String
    let worktreeName: String?
    let targets: [String]
    let pid: Int
    let state: BuildState
    let startTime: Date
    let endTime: Date?
    let exitCode: Int
    let source: BuildSource
    let elapsedMS: Int64
    let cacheHitRatio: Double?
    let profileURL: String?

    var id: String { invocationID }

    init(invocationID: String, worktree: String, worktreeName: String?, targets: [String],
         pid: Int, state: BuildState, startTime: Date, endTime: Date?, exitCode: Int,
         source: BuildSource, elapsedMS: Int64, cacheHitRatio: Double?, profileURL: String?) {
        self.invocationID = invocationID
        self.worktree = worktree
        self.worktreeName = worktreeName
        self.targets = targets
        self.pid = pid
        self.state = state
        self.startTime = startTime
        self.endTime = endTime
        self.exitCode = exitCode
        self.source = source
        self.elapsedMS = elapsedMS
        self.cacheHitRatio = cacheHitRatio
        self.profileURL = profileURL
    }

    enum CodingKeys: String, CodingKey {
        case invocationID = "invocation_id"
        case worktree
        case worktreeName = "worktree_name"
        case targets
        case pid
        case state
        case startTime = "start_time"
        case endTime = "end_time"
        case exitCode = "exit_code"
        case source
        case elapsedMS = "elapsed_ms"
        case cacheHitRatio = "cache_hit_ratio"
        case profileURL = "profile_url"
    }

    /// Display name for the menu row: the daemon-supplied `worktree_name` when set,
    /// else the last path component of the absolute `worktree`.
    var displayName: String {
        if let name = worktreeName, !name.isEmpty { return name }
        let trimmed = worktree.hasSuffix("/") ? String(worktree.dropLast()) : worktree
        return (trimmed as NSString).lastPathComponent
    }

    /// A build still under the daemon's control — the only state where killing makes sense.
    var isActive: Bool { state == .running || state == .queued }
}

/// Mirrors the `State*` consts in `internal/api/api.go`. The decoder maps any
/// unrecognized raw value to `.unknown` so a newer daemon adding a state never
/// crashes this thin client (forward-compat, the only defensive code it needs).
enum BuildState: String, Codable, UnknownFallbackDecodable {
    case queued, running, finished, failed, killed, gone, unknown
    static var unknownCase: BuildState { .unknown }
}

/// Mirrors the `Source*` consts: `registered` | `discovered`. Unknown → `.unknown`.
enum BuildSource: String, Codable, UnknownFallbackDecodable {
    case registered, discovered, unknown
    static var unknownCase: BuildSource { .unknown }
}
