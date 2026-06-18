import Foundation

/// Typed transport errors surfaced to the store so it can distinguish "feature not
/// landed yet" (501, owning epic not merged) from a genuine connection failure.
enum BrokerClientError: Error {
    case http(status: Int)
    case notImplemented   // 501: route reserved for E3 (kill) / E4 (metrics)
    case badResponse
}

/// Isolates all networking state: the HTTP requests and the WS upgrade. A pure
/// projection layer — no business logic, no caching to disk. Paths/verbs are taken
/// verbatim from the frozen contract (`internal/api`, E2 §4.2):
///   GET  /builds                       → {"builds":[…]}
///   POST /builds/{invocation_id}/kill  → KillResult (501 until E3)
///   WS   /events                       → snapshot then build events
actor BrokerClient {
    private let config: BrokerConfig
    private let session: URLSession
    private let decoder = BrokerDecoder.shared
    private var wsTask: URLSessionWebSocketTask?

    init(config: BrokerConfig, session: URLSession = URLSession(configuration: .ephemeral)) {
        self.config = config
        self.session = session
    }

    // MARK: HTTP

    /// Initial snapshot. Decodes the `{"builds":[…]}` wrapper and returns `.builds`.
    func fetchBuilds() async throws -> [Build] {
        try await get("/builds", as: BuildsResponse.self).builds
    }

    /// POST /builds/{invocation_id}/kill — fire the kill intent. A 501 (E3 not landed)
    /// is surfaced as `.notImplemented`; the daemon owns the actual kill, so the row
    /// only leaves when the authoritative `build` event arrives in a terminal state.
    func kill(invocationID: String) async throws {
        let path = "/builds/\(invocationID)/kill"
        var req = request(path: path)
        req.httpMethod = "POST"
        let (_, resp) = try await session.data(for: req)
        try Self.checkStatus(resp)
    }

    private func get<T: Decodable>(_ path: String, as: T.Type) async throws -> T {
        let (data, resp) = try await session.data(for: request(path: path))
        try Self.checkStatus(resp)
        return try decoder.decode(T.self, from: data)
    }

    private func request(path: String) -> URLRequest {
        var r = URLRequest(url: BrokerEndpoints.http(config, path))
        r.setValue("Bearer \(config.token)", forHTTPHeaderField: "Authorization")
        return r
    }

    private static func checkStatus(_ resp: URLResponse) throws {
        guard let http = resp as? HTTPURLResponse else { throw BrokerClientError.badResponse }
        switch http.statusCode {
        case 200...299: return
        case 501: throw BrokerClientError.notImplemented
        default: throw BrokerClientError.http(status: http.statusCode)
        }
    }

    // MARK: WebSocket

    /// Live event stream. The first frame is a `snapshot` (E2's resync contract),
    /// then incremental `build` events. Ping/pong keepalive is handled by
    /// `URLSessionWebSocketTask`; the app never decodes a heartbeat frame. The stream
    /// finishes (optionally throwing) when the socket closes — the store schedules a
    /// backoff reconnect.
    func events() -> AsyncThrowingStream<Event, Error> {
        AsyncThrowingStream { continuation in
            let task = Task { [weak self] in await self?.runEventLoop(continuation) }
            continuation.onTermination = { _ in task.cancel() }
        }
    }

    private func runEventLoop(_ cont: AsyncThrowingStream<Event, Error>.Continuation) async {
        var req = URLRequest(url: BrokerEndpoints.ws(config, "/events"))
        // The bearer token authenticates the WS HTTP upgrade.
        // `URLSessionWebSocketTask` sends custom upgrade headers (unlike the browser
        // WebSocket API). If E2 ever 401s the upgrade because `Authorization` was
        // dropped, the documented fallback is the `Sec-WebSocket-Protocol` token shim.
        req.setValue("Bearer \(config.token)", forHTTPHeaderField: "Authorization")
        let task = session.webSocketTask(with: req)
        wsTask = task
        task.resume()
        do {
            while !Task.isCancelled {
                let msg = try await task.receive()
                if let event = decode(msg) {
                    cont.yield(event)
                }
            }
            cont.finish()
        } catch {
            cont.finish(throwing: error)
        }
        wsTask = nil
    }

    private func decode(_ msg: URLSessionWebSocketTask.Message) -> Event? {
        let data: Data
        switch msg {
        case .string(let s): data = Data(s.utf8)
        case .data(let d): data = d
        @unknown default: return nil
        }
        return try? decoder.decode(Event.self, from: data)
    }

    func close() {
        wsTask?.cancel(with: .goingAway, reason: nil)
        wsTask = nil
    }
}
