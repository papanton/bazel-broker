import XCTest
@testable import BrokerMenuBar

/// Verifies the HTTP client hits the FROZEN routes/verbs/headers (no live broker
/// needed): a stub `URLProtocol` captures each request and replies from the fixtures.
final class BrokerClientTests: XCTestCase {
    private var session: URLSession!
    private let config = BrokerConfig(host: "127.0.0.1", port: 8765, token: "tok123")

    override func setUp() {
        super.setUp()
        let cfg = URLSessionConfiguration.ephemeral
        cfg.protocolClasses = [StubURLProtocol.self]
        session = URLSession(configuration: cfg)
        StubURLProtocol.reset()
    }

    func testFetchBuildsHitsGETBuildsAndUnwraps() async throws {
        StubURLProtocol.responder = { req in
            XCTAssertEqual(req.url?.path, "/builds")
            XCTAssertEqual(req.httpMethod, "GET")
            XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer tok123")
            return (200, (try? Fixtures.data("builds.json")) ?? Data())
        }
        let client = BrokerClient(config: config, session: session)
        let builds = try await client.fetchBuilds()
        XCTAssertEqual(builds.count, 2)
        XCTAssertEqual(builds.first?.invocationID, "a1b2")
    }

    func testKillHitsNestedPerBuildRoute() async throws {
        var seenPath: String?
        var seenMethod: String?
        StubURLProtocol.responder = { req in
            seenPath = req.url?.path
            seenMethod = req.httpMethod
            XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer tok123")
            return (200, Data(#"{"killed":true,"invocation_id":"a1b2","pid":1,"outcome":"sigint","elapsed_ms":1}"#.utf8))
        }
        let client = BrokerClient(config: config, session: session)
        try await client.kill(invocationID: "a1b2")
        // FROZEN: POST /builds/{invocation_id}/kill — NOT POST /kill.
        XCTAssertEqual(seenPath, "/builds/a1b2/kill")
        XCTAssertEqual(seenMethod, "POST")
    }

    func testNotImplementedIsTypedNotAConnectionError() async {
        StubURLProtocol.responder = { _ in (501, Data(#"{"error":"not_implemented","epic":"E3"}"#.utf8)) }
        let client = BrokerClient(config: config, session: session)
        do {
            try await client.kill(invocationID: "a1b2")
            XCTFail("expected notImplemented")
        } catch BrokerClientError.notImplemented {
            // expected: route reserved until E3 lands.
        } catch {
            XCTFail("expected .notImplemented, got \(error)")
        }
    }

    func testUnauthorizedSurfacesHTTP401() async {
        StubURLProtocol.responder = { _ in (401, Data(#"{"error":"unauthorized"}"#.utf8)) }
        let client = BrokerClient(config: config, session: session)
        do {
            _ = try await client.fetchBuilds()
            XCTFail("expected http 401")
        } catch BrokerClientError.http(let status) {
            XCTAssertEqual(status, 401)
        } catch {
            XCTFail("unexpected \(error)")
        }
    }
}

/// In-process HTTP stub so client route tests never touch the network.
final class StubURLProtocol: URLProtocol {
    nonisolated(unsafe) static var responder: ((URLRequest) -> (Int, Data))?

    static func reset() { responder = nil }

    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }

    override func startLoading() {
        guard let responder = Self.responder else {
            client?.urlProtocol(self, didFailWithError: URLError(.badServerResponse))
            return
        }
        let (status, body) = responder(request)
        let resp = HTTPURLResponse(url: request.url!, statusCode: status,
                                   httpVersion: "HTTP/1.1", headerFields: nil)!
        client?.urlProtocol(self, didReceive: resp, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: body)
        client?.urlProtocolDidFinishLoading(self)
    }

    override func stopLoading() {}
}
