import XCTest
@testable import ConductorKit

final class SSETests: XCTestCase {
    private func decode(_ chunks: [String]) -> [SSEEvent] {
        var d = SSEDecoder()
        return chunks.flatMap { d.decode(Data($0.utf8)) }
    }

    func testBlankLineEndsEachEvent() {
        let events = decode(["event: task.claimed\ndata: {\"a\":1}\n\nevent: conflict.detected\ndata: {\"b\":2}\n\n"])
        XCTAssertEqual(events, [
            SSEEvent(event: "task.claimed", data: "{\"a\":1}"),
            SSEEvent(event: "conflict.detected", data: "{\"b\":2}"),
        ])
    }

    func testNoEventWithoutTheBlankLine() {
        // The trap `bytes.lines` falls into: without the empty line nothing is dispatched.
        XCTAssertEqual(decode(["event: x\ndata: 1\n"]), [])
        var d = SSEDecoder()
        _ = d.decode(Data("event: x\ndata: 1\n".utf8))
        d.finish()
        XCTAssertEqual(d.decode(Data()), [], "an incomplete event at the end of the stream is discarded")
    }

    func testEmptyLinesAreKeptByTheSplitter() {
        var s = SSELineSplitter()
        XCTAssertEqual(s.take(Array("a\n\nb\r\n\r\nc\r\rd\n".utf8)), ["a", "", "b", "", "c", "", "d"])
    }

    func testCRLFSplitAcrossChunks() {
        let events = decode(["data: one\r", "\n\r", "\ndata: two\r\n", "\r\n"])
        XCTAssertEqual(events.map(\.data), ["one", "two"])
    }

    func testLoneCarriageReturns() {
        XCTAssertEqual(decode(["data: a\r\rdata: b\r\r"]).map(\.data), ["a", "b"])
    }

    func testCommentsAndKeepalivesAreSkipped() {
        let events = decode([": keepalive\n\n", "data: x\n: in the middle\n\n"])
        XCTAssertEqual(events, [SSEEvent(event: nil, data: "x")])
        XCTAssertEqual(events.first?.type, "message")
    }

    func testMultiLineDataJoinsWithNewlines() {
        XCTAssertEqual(decode(["data: line one\ndata:line two\ndata\n\n"]).first?.data, "line one\nline two\n")
    }

    func testEventNameDoesNotLeakIntoTheNextEvent() {
        let events = decode(["event: a\n\ndata: 1\n\nevent: b\ndata: 2\n\ndata: 3\n\n"])
        XCTAssertEqual(events, [SSEEvent(event: nil, data: "1"), SSEEvent(event: "b", data: "2"), SSEEvent(event: nil, data: "3")])
    }

    func testOnlyOneLeadingSpaceIsStripped() {
        XCTAssertEqual(decode(["data:  two spaces\n\n"]).first?.data, " two spaces")
    }

    func testUTF8SplitAcrossChunksIsWhole() {
        let bytes = Array("data: héllo ✓\n\n".utf8)
        var d = SSEDecoder()
        var events: [SSEEvent] = []
        for b in bytes { events += d.decode(Data([b])) }
        XCTAssertEqual(events.first?.data, "héllo ✓")
    }

    func testByteOrderMarkAtTheStart() {
        XCTAssertEqual(decode(["\u{FEFF}data: x\n\n"]).first?.data, "x")
    }

    func testIdAndRetry() {
        var d = SSEDecoder()
        let events = d.decode(Data("id: 42\nretry: 5000\ndata: x\n\n".utf8))
        XCTAssertEqual(events.first?.id, "42")
        XCTAssertEqual(d.lastEventID, "42")
        XCTAssertEqual(d.retryMilliseconds, 5000)
    }

    func testFieldWithoutColon() {
        XCTAssertEqual(decode(["data\n\n"]).first?.data, "")
    }

    /// Exactly what conductord's streamEvents writes: `event: <type>\ndata: <json>\n\n`.
    func testConductordFrameDecodesToADomainEvent() throws {
        let frame = """
        event: quota.exhausted
        data: {"id":"e1","organization_id":"o","project_id":"p","aggregate_type":"quota","aggregate_id":"a","sequence_number":3,"type":"quota.exhausted","visibility":"team_summary","payload":{"harness":"claude","kind":"five_hour","severity":"exhausted","reason":"usage_limit","percent_hint":100},"occurred_at":"2026-10-07T10:00:00Z"}


        """
        let events = decode([frame])
        XCTAssertEqual(events.count, 1)
        let e = try XCTUnwrap(DomainEvent.decode(events[0]))
        XCTAssertEqual(e.type, "quota.exhausted")
        XCTAssertEqual(e.payload?["harness"]?.stringValue, "claude")
        XCTAssertEqual(e.payload?["percent_hint"]?.intValue, 100)
    }

    func testReconnectPolicyBacksOffAndResets() {
        var p = ReconnectPolicy(delays: [1, 2, 5])
        XCTAssertEqual(p.nextDelay(), 1)
        XCTAssertEqual(p.nextDelay(), 2)
        XCTAssertEqual(p.nextDelay(), 5)
        XCTAssertEqual(p.nextDelay(), 5)
        XCTAssertEqual(p.nextDelay(serverRetryMilliseconds: 9000), 9)
        p.reset()
        XCTAssertEqual(p.nextDelay(), 1)
    }

    func testStreamResponseVerdicts() {
        XCTAssertEqual(EventStream.verdict(status: 200, contentType: "text/event-stream"), .stream)
        XCTAssertEqual(EventStream.verdict(status: 200, contentType: "text/event-stream; charset=utf-8"), .stream)
        XCTAssertEqual(EventStream.verdict(status: 401, contentType: "application/json"), .unauthorized)
        XCTAssertEqual(EventStream.verdict(status: 503, contentType: "application/json"), .refused(503))
        XCTAssertEqual(EventStream.verdict(status: 200, contentType: "application/json"), .refused(200))
    }

    func testStreamRequestCarriesTheTokenInAHeader() {
        let s = EventStream(url: URL(string: "http://127.0.0.1:8080/v1/projects/p/events/stream")!, token: "cdt_x", onEvent: { _ in })
        let req = s.makeRequest()
        XCTAssertEqual(req.value(forHTTPHeaderField: "Authorization"), "Bearer cdt_x")
        XCTAssertEqual(req.value(forHTTPHeaderField: "Accept"), "text/event-stream")
        XCTAssertFalse(req.url!.absoluteString.contains("token"))
    }
}
