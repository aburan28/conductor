import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

/// How long to wait before reconnecting: quickly at first, then backing off, and never less
/// than what the server asked for with `retry:`.
public struct ReconnectPolicy: Equatable, Sendable {
    public var delays: [TimeInterval]
    private var attempt = 0

    public init(delays: [TimeInterval] = [1, 2, 5, 10, 30]) {
        self.delays = delays
    }

    public mutating func nextDelay(serverRetryMilliseconds: Int? = nil) -> TimeInterval {
        let base = delays[min(attempt, delays.count - 1)]
        attempt += 1
        if let ms = serverRetryMilliseconds {
            return max(base, TimeInterval(ms) / 1000)
        }
        return base
    }

    /// A connection that delivered something: the next failure starts from the shortest wait.
    public mutating func reset() { attempt = 0 }
}

/// The SSE feed of one project, `GET /v1/projects/{p}/events/stream`, reconnecting on its own.
///
/// Built on a data delegate rather than `URLSession.bytes(for:).lines`, which drops the blank
/// line that ends every event (see `SSELineSplitter`), and which does not exist in Linux
/// Foundation, where this is tested. Callbacks arrive on a private serial queue.
public final class EventStream: NSObject, URLSessionDataDelegate, @unchecked Sendable {
    public enum State: Equatable, Sendable {
        case connecting
        case open
        /// Waiting `retryIn` seconds after a failure, with why.
        case waiting(retryIn: TimeInterval, reason: String)
        /// The token was refused; the owner must sign in again and start a new stream.
        case unauthorized
        case stopped
    }

    public let url: URL
    private let token: String?
    private let onEvent: @Sendable (SSEEvent) -> Void
    private let onState: @Sendable (State) -> Void

    private let queue: OperationQueue
    private let lock = NSLock()
    private var session: URLSession?
    private var task: URLSessionDataTask?
    private var decoder = SSEDecoder()
    private var policy: ReconnectPolicy
    private var stopped = false
    private var opened = false
    private var lastEventID: String?

    public init(url: URL, token: String?, policy: ReconnectPolicy = ReconnectPolicy(),
                onEvent: @escaping @Sendable (SSEEvent) -> Void,
                onState: @escaping @Sendable (State) -> Void = { _ in }) {
        self.url = url
        self.token = token
        self.policy = policy
        self.onEvent = onEvent
        self.onState = onState
        queue = OperationQueue()
        queue.maxConcurrentOperationCount = 1
        queue.name = "dev.conductor.event-stream"
        super.init()
    }

    /// The request a connection makes: the bearer token in a header (never the query, where
    /// the stream route also accepts one, because a URL ends up in logs), and the last event
    /// id seen when reconnecting.
    public func makeRequest() -> URLRequest {
        lock.lock()
        let last = lastEventID
        lock.unlock()
        return makeRequest(lastEventID: last)
    }

    /// Pure, and takes no lock: `connect` builds the request while it holds the lock.
    func makeRequest(lastEventID: String?) -> URLRequest {
        var req = URLRequest(url: url)
        req.httpMethod = "GET"
        req.setValue("text/event-stream", forHTTPHeaderField: "Accept")
        req.setValue("no-cache", forHTTPHeaderField: "Cache-Control")
        if let token, !token.isEmpty { req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization") }
        if let lastEventID { req.setValue(lastEventID, forHTTPHeaderField: "Last-Event-ID") }
        return req
    }

    public func start() {
        lock.lock()
        stopped = false
        if session == nil {
            let config = URLSessionConfiguration.ephemeral
            // An idle timeout, not a total one: conductord writes a keepalive comment every
            // 20 seconds, so a minute and a half of silence means the connection is gone.
            config.timeoutIntervalForRequest = 90
            config.timeoutIntervalForResource = 365 * 24 * 3600
            config.requestCachePolicy = .reloadIgnoringLocalCacheData
            session = URLSession(configuration: config, delegate: self, delegateQueue: queue)
        }
        lock.unlock()
        connect()
    }

    public func stop() {
        lock.lock()
        stopped = true
        let s = session
        session = nil
        task = nil
        lock.unlock()
        // The session holds its delegate (this object) until it is invalidated.
        s?.invalidateAndCancel()
        onState(.stopped)
    }

    private func connect() {
        lock.lock()
        guard !stopped, let session else { lock.unlock(); return }
        decoder = SSEDecoder()
        opened = false
        let t = session.dataTask(with: makeRequest(lastEventID: lastEventID))
        task = t
        lock.unlock()
        onState(.connecting)
        t.resume()
    }

    private func scheduleReconnect(_ reason: String) {
        lock.lock()
        if stopped { lock.unlock(); return }
        let delay = policy.nextDelay(serverRetryMilliseconds: decoder.retryMilliseconds)
        lock.unlock()
        onState(.waiting(retryIn: delay, reason: reason))
        DispatchQueue.global().asyncAfter(deadline: .now() + delay) { [weak self] in
            self?.connect()
        }
    }

    /// What a response's status and type mean for the stream.
    public enum ResponseVerdict: Equatable, Sendable {
        case stream
        case unauthorized
        case refused(Int)
    }

    public static func verdict(status: Int, contentType: String?) -> ResponseVerdict {
        if status == 401 { return .unauthorized }
        if status == 200, (contentType ?? "").lowercased().contains("text/event-stream") { return .stream }
        return .refused(status)
    }

    private static func verdict(of task: URLSessionTask) -> ResponseVerdict? {
        guard let http = task.response as? HTTPURLResponse else { return nil }
        return verdict(status: http.statusCode, contentType: http.value(forHTTPHeaderField: "Content-Type"))
    }

    private func becameUnauthorized() {
        lock.lock()
        stopped = true
        lock.unlock()
        onState(.unauthorized)
    }

    // MARK: URLSessionDataDelegate
    //
    // Only the two delegate methods without completion handlers are implemented, and the
    // response is judged from the task: the response method's handler type has changed
    // between SDKs (it gained @Sendable), and a witness that no longer matches is silently
    // never called.

    public func urlSession(_ session: URLSession, dataTask: URLSessionDataTask, didReceive data: Data) {
        lock.lock()
        guard task === dataTask else { lock.unlock(); return }
        lock.unlock()
        switch Self.verdict(of: dataTask) {
        case .unauthorized?:
            dataTask.cancel()
            becameUnauthorized()
            return
        case .refused?, nil:
            // An error page, not a stream; the completion that follows schedules a retry.
            dataTask.cancel()
            return
        case .stream?:
            break
        }
        lock.lock()
        let first = !opened
        opened = true
        let events = decoder.decode(data)
        if !events.isEmpty { policy.reset() }
        if let id = decoder.lastEventID { lastEventID = id }
        lock.unlock()
        if first { onState(.open) }
        for e in events { onEvent(e) }
    }

    public func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        lock.lock()
        let current = self.task === task
        if current {
            decoder.finish()
            opened = false
        }
        let isStopped = stopped
        lock.unlock()
        guard current, !isStopped else { return }
        if Self.verdict(of: task) == .unauthorized {
            becameUnauthorized()
            return
        }
        var reason = error?.localizedDescription ?? "the stream ended"
        if case .refused(let status)? = Self.verdict(of: task) {
            reason = "the control plane answered \(status)"
        }
        scheduleReconnect(reason)
    }
}
