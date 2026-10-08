import Foundation

/// One server-sent event: the `event:` name (nil means the default, "message"), the `data:`
/// lines joined with newlines, and the `id:` when the server sent one.
public struct SSEEvent: Equatable, Sendable {
    public var event: String?
    public var data: String
    public var id: String?

    public init(event: String? = nil, data: String, id: String? = nil) {
        self.event = event
        self.data = data
        self.id = id
    }

    /// The event's type as the stream names it: conductord sends `event: <domain event type>`.
    public var type: String { event ?? "message" }
}

/// Bytes to lines for text/event-stream, keeping the empty ones.
///
/// An empty line is what ends an SSE event. `URLSession.bytes(for:).lines` (and most line
/// readers) skip empty lines, so every event of a stream read through them runs into the
/// next and nothing is ever dispatched. This splitter returns them. A line ends at LF, CRLF
/// or a lone CR, which is what the framing allows, and a CRLF split across two chunks is
/// still one line ending.
public struct SSELineSplitter: Sendable {
    private var buffer: [UInt8] = []
    private var afterCR = false

    public init() {}

    /// The line `byte` completes, if it completes one.
    public mutating func take(_ byte: UInt8) -> String? {
        switch byte {
        case 0x0A:
            if afterCR {
                // The LF of a CRLF whose CR already ended the line.
                afterCR = false
                return nil
            }
            return emit()
        case 0x0D:
            afterCR = true
            return emit()
        default:
            afterCR = false
            buffer.append(byte)
            return nil
        }
    }

    /// Every line a chunk of bytes completes, in order.
    public mutating func take<S: Sequence>(_ bytes: S) -> [String] where S.Element == UInt8 {
        var lines: [String] = []
        for b in bytes {
            if let line = take(b) { lines.append(line) }
        }
        return lines
    }

    /// A last line the stream ended without terminating.
    public mutating func finish() -> String? {
        afterCR = false
        return buffer.isEmpty ? nil : emit()
    }

    private mutating func emit() -> String {
        defer { buffer.removeAll(keepingCapacity: true) }
        // Bytes, not characters, are buffered, so a UTF-8 sequence split across chunks is
        // whole by the time the line is decoded.
        return String(decoding: buffer, as: UTF8.self)
    }
}

/// The text/event-stream framing, fed one line at a time without its line ending: fields
/// accumulate until a blank line dispatches them. Comments (`: keepalive`, which conductord
/// sends every 20 seconds) and unknown fields are skipped.
public struct SSEParser: Sendable {
    private var event: String?
    private var data: [String] = []
    private var sawData = false
    private var firstLine = true
    /// The last `id:` seen, which a reconnect sends back as `Last-Event-ID`.
    public private(set) var lastEventID: String?
    /// The server's `retry:` advice, in milliseconds.
    public private(set) var retryMilliseconds: Int?

    public init() {}

    public mutating func feed(_ rawLine: String) -> SSEEvent? {
        var line = rawLine
        if firstLine {
            firstLine = false
            // A UTF-8 byte order mark at the very start of the stream is not part of a field.
            if line.hasPrefix("\u{FEFF}") { line.removeFirst() }
        }
        if line.isEmpty { return dispatch() }
        if line.hasPrefix(":") { return nil }
        let field: Substring
        var value: Substring
        if let colon = line.firstIndex(of: ":") {
            field = line[..<colon]
            value = line[line.index(after: colon)...]
            if value.hasPrefix(" ") { value = value.dropFirst() }
        } else {
            field = Substring(line)
            value = ""
        }
        switch field {
        case "event":
            event = String(value)
        case "data":
            data.append(String(value))
            sawData = true
        case "id":
            // An id containing NUL is ignored, per the specification.
            if !value.contains("\u{0}") { lastEventID = String(value) }
        case "retry":
            if let ms = Int(value), ms >= 0 { retryMilliseconds = ms }
        default:
            break
        }
        return nil
    }

    /// A blank line: the event so far, if it had any data. An event with no data lines is
    /// discarded, as the specification says, and its name is forgotten with it.
    private mutating func dispatch() -> SSEEvent? {
        defer {
            event = nil
            data = []
            sawData = false
        }
        guard sawData else { return nil }
        return SSEEvent(event: event, data: data.joined(separator: "\n"), id: lastEventID)
    }
}

/// Splitter and parser together: chunks of bytes in, whole events out.
public struct SSEDecoder: Sendable {
    private var lines = SSELineSplitter()
    private var parser = SSEParser()

    public init() {}

    public var lastEventID: String? { parser.lastEventID }
    public var retryMilliseconds: Int? { parser.retryMilliseconds }

    public mutating func decode(_ chunk: Data) -> [SSEEvent] {
        var events: [SSEEvent] = []
        for line in lines.take(chunk) {
            if let e = parser.feed(line) { events.append(e) }
        }
        return events
    }

    /// The stream ended. An event still waiting for its blank line is incomplete and is not
    /// dispatched, per the specification; a last unterminated line is still parsed so its
    /// `id:` or `retry:` counts.
    public mutating func finish() {
        if let line = lines.finish() { _ = parser.feed(line) }
    }
}
