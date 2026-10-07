import XCTest
@testable import ConductorKit

final class InviteTests: XCTestCase {
    /// Byte for byte what cmd/conductor/invite.go joinLink prints for the same input (taken
    /// from running it).
    func testWebLinkMatchesConductorInvite() {
        XCTAssertEqual(InviteLink.web(endpoint: "https://mac.tail1234.ts.net/", project: "my repo/ä", token: "cdt_ab+c/d=e~f_g-h.i"),
                       "https://mac.tail1234.ts.net/#project=my+repo%2F%C3%A4&token=cdt_ab%2Bc%2Fd%3De~f_g-h.i")
        XCTAssertEqual(InviteLink.web(endpoint: "http://127.0.0.1:8080", project: "conductor", token: "cdt_0123456789abcdef"),
                       "http://127.0.0.1:8080/#project=conductor&token=cdt_0123456789abcdef")
    }

    func testAppLinkCarriesTheEndpoint() {
        XCTAssertEqual(InviteLink.app(endpoint: "https://mac.tail1234.ts.net/", project: "conductor", token: "cdt_x"),
                       "conductor://join#endpoint=https%3A%2F%2Fmac.tail1234.ts.net&project=conductor&token=cdt_x")
        XCTAssertEqual(InviteLink.shareText("L"), "Join my Conductor swarm: L")
    }

    func testParseAppLink() throws {
        let link = InviteLink.app(endpoint: "https://mac.tail1234.ts.net", project: "my repo", token: "cdt_ab+c")
        let join = try InviteLink.parse(link).get()
        XCTAssertEqual(join, .init(endpoint: "https://mac.tail1234.ts.net", project: "my repo", token: "cdt_ab+c"))
        XCTAssertEqual(join.webLink, "https://mac.tail1234.ts.net/#project=my+repo&token=cdt_ab%2Bc")
    }

    func testParseAppLinkInTheQueryAndWithoutSlashes() throws {
        XCTAssertEqual(try InviteLink.parse("conductor://join?endpoint=http://10.0.0.5:8080&token=t1").get().endpoint, "http://10.0.0.5:8080")
        XCTAssertEqual(try InviteLink.parse("conductor:join#endpoint=https%3A%2F%2Fa.b&token=t").get().token, "t")
    }

    func testParseWebLink() throws {
        let join = try InviteLink.parse("https://conductor.team:8443/#project=myrepo&token=cdt_1").get()
        XCTAssertEqual(join, .init(endpoint: "https://conductor.team:8443", project: "myrepo", token: "cdt_1"))
    }

    func testParseRefusals() {
        XCTAssertEqual(InviteLink.parse("conductor://join#endpoint=https%3A%2F%2Fa.b"), .failure(.noToken))
        XCTAssertEqual(InviteLink.parse("conductor://join#token=t"), .failure(.noEndpoint))
        XCTAssertEqual(InviteLink.parse("conductor://settings#token=t"), .failure(.notAJoinLink))
        XCTAssertEqual(InviteLink.parse("conductor://join#endpoint=file%3A%2F%2F%2Fetc&token=t"), .failure(.badEndpoint("file:///etc")))
        XCTAssertEqual(InviteLink.parse("smb://server/share#token=t"), .failure(.notAJoinLink))
        XCTAssertEqual(InviteLink.parse("not a link"), .failure(.notAJoinLink))
    }

    func testLoopback() {
        XCTAssertTrue(InviteLink.isLoopback("http://127.0.0.1:8080"))
        XCTAssertTrue(InviteLink.isLoopback("http://localhost:8080"))
        XCTAssertTrue(InviteLink.isLoopback("http://[::1]:8080"))
        XCTAssertFalse(InviteLink.isLoopback("https://mac.tail1234.ts.net"))
    }

    func testInviteRequestBody() throws {
        let body = try JSONValue.decode(try JSONEncoder().encode(InviteRequest(handle: "rachel", role: "contributor", tokenTTL: "168h")))
        XCTAssertEqual(body, .object(["handle": .string("rachel"), "role": .string("contributor"), "kind": .string("human"), "token_ttl": .string("168h")]))
        let defaulted = try JSONValue.decode(try JSONEncoder().encode(InviteRequest(handle: "r", role: "reviewer", tokenTTL: nil)))
        XCTAssertNil(defaulted["token_ttl"])
        XCTAssertEqual(InviteExpiry.serverDefault.ttl, nil)
        XCTAssertEqual(InviteExpiry.week.ttl, "168h")
    }

    // MARK: - reachability

    func testTailscaleStatus() {
        let running = #"{"BackendState":"Running","Self":{"DNSName":"ada-mac.tail1234.ts.net.","HostName":"ada-mac"}}"#
        XCTAssertEqual(Tailscale.magicDNSName(statusJSON: Data(running.utf8)), "ada-mac.tail1234.ts.net")
        let stopped = #"{"BackendState":"Stopped","Self":{"DNSName":"ada-mac.tail1234.ts.net."}}"#
        XCTAssertNil(Tailscale.magicDNSName(statusJSON: Data(stopped.utf8)))
        XCTAssertNil(Tailscale.magicDNSName(statusJSON: Data("not json".utf8)))
        XCTAssertEqual(Tailscale.serveArguments(port: 8080), ["serve", "--bg", "8080"])
    }

    func testTailscaleServeStatus() {
        let serving = #"{"TCP":{"443":{"HTTPS":true}},"Web":{"ada-mac.tail1234.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:8080"}}}}}"#
        XCTAssertTrue(Tailscale.isServing(port: 8080, serveStatusJSON: Data(serving.utf8)))
        XCTAssertFalse(Tailscale.isServing(port: 8081, serveStatusJSON: Data(serving.utf8)))
        XCTAssertFalse(Tailscale.isServing(port: 8080, serveStatusJSON: Data("{}".utf8)))
    }

    func testReachabilityOrder() {
        XCTAssertEqual(InviteReachability.decide(publicURL: "https://conductor.team", tailscaleName: "m.ts.net", tailscaleServing: true),
                       .publicURL("https://conductor.team"))
        XCTAssertEqual(InviteReachability.decide(publicURL: "http://127.0.0.1:8080", tailscaleName: "m.ts.net", tailscaleServing: false),
                       .tailscaleAvailable(name: "m.ts.net"))
        let serving = InviteReachability.decide(publicURL: "", tailscaleName: "m.ts.net", tailscaleServing: true)
        XCTAssertEqual(serving, .tailscaleServing(endpoint: "https://m.ts.net"))
        XCTAssertEqual(serving.endpoint, "https://m.ts.net")
        let local = InviteReachability.decide(publicURL: nil, tailscaleName: nil, tailscaleServing: false)
        XCTAssertEqual(local, .localOnly)
        XCTAssertNil(local.endpoint)
    }
}
