import Foundation
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif

public enum APIError: Error, Equatable, CustomStringConvertible {
    /// The server answered with an error body.
    case server(status: Int, code: String?, message: String)
    /// No answer: the daemon is down or unreachable.
    case transport(String)
    /// An answer this app could not read.
    case decoding(String)

    public var description: String {
        switch self {
        case .server(let status, _, let message): return message.isEmpty ? "the control plane answered \(status)" : message
        case .transport(let why): return why
        case .decoding(let why): return "could not read the control plane's answer: \(why)"
        }
    }

    public var status: Int? {
        if case .server(let s, _, _) = self { return s }
        return nil
    }

    public var isUnauthenticated: Bool { status == 401 }
}

/// conductord's HTTP API, as much of it as the app calls. Plain URLSession and Codable.
///
/// Requests carry no `Origin` header (URLSession adds none), come from loopback, and name a
/// loopback Host, which is what `POST /v1/local/session` requires before it signs the
/// machine's owner in without a token (internal/api/local.go).
public struct APIClient: Sendable {
    public var endpoint: URL
    public var token: String?
    private let session: URLSession

    public init(endpoint: URL, token: String? = nil, session: URLSession? = nil) {
        self.endpoint = endpoint
        self.token = token
        if let session {
            self.session = session
        } else {
            let config = URLSessionConfiguration.ephemeral
            config.timeoutIntervalForRequest = 10
            config.httpCookieAcceptPolicy = .never
            config.requestCachePolicy = .reloadIgnoringLocalCacheData
            self.session = URLSession(configuration: config)
        }
    }

    public func withToken(_ token: String?) -> APIClient {
        var copy = self
        copy.token = token
        return copy
    }

    // MARK: - requests

    /// A request for `path` (which may carry a query), JSON in and out.
    public func request(_ method: String, _ path: String, body: Data? = nil) -> URLRequest {
        let base = endpoint.absoluteString.hasSuffix("/") ? String(endpoint.absoluteString.dropLast()) : endpoint.absoluteString
        var req = URLRequest(url: URL(string: base + path)!)
        req.httpMethod = method
        req.setValue("application/json", forHTTPHeaderField: "Accept")
        if let body {
            req.httpBody = body
            req.setValue("application/json", forHTTPHeaderField: "Content-Type")
        }
        if let token, !token.isEmpty {
            req.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        }
        return req
    }

    /// Percent-encodes one path segment (a project slug, an id).
    public static func segment(_ s: String) -> String {
        var allowed = CharacterSet.alphanumerics
        allowed.insert(charactersIn: "-._~")
        return s.addingPercentEncoding(withAllowedCharacters: allowed) ?? s
    }

    public func send(_ req: URLRequest) async throws -> (Data, Int) {
        try await withCheckedThrowingContinuation { (continuation: CheckedContinuation<(Data, Int), Error>) in
            let task = session.dataTask(with: req) { data, response, error in
                if let error {
                    continuation.resume(throwing: APIError.transport(error.localizedDescription))
                    return
                }
                let status = (response as? HTTPURLResponse)?.statusCode ?? 0
                continuation.resume(returning: (data ?? Data(), status))
            }
            task.resume()
        }
    }

    public func call<T: Decodable>(_ method: String, _ path: String, body: Data? = nil, as type: T.Type) async throws -> T {
        let (data, status) = try await send(request(method, path, body: body))
        return try Self.decodeResponse(data: data, status: status, as: type)
    }

    /// The body as `T` on a 2xx, else the server's error body as an `APIError`.
    public static func decodeResponse<T: Decodable>(data: Data, status: Int, as type: T.Type) throws -> T {
        guard (200..<300).contains(status) else {
            if let body = try? JSONDecoder().decode(APIErrorBody.self, from: data) {
                throw APIError.server(status: status, code: body.code, message: body.error)
            }
            let text = String(decoding: data.prefix(500), as: UTF8.self).trimmingCharacters(in: .whitespacesAndNewlines)
            throw APIError.server(status: status, code: nil, message: text)
        }
        do {
            return try JSONDecoder().decode(T.self, from: data)
        } catch {
            throw APIError.decoding(String(describing: error))
        }
    }

    private static func json<T: Encodable>(_ value: T) -> Data {
        (try? JSONEncoder().encode(value)) ?? Data("{}".utf8)
    }

    // MARK: - endpoints

    /// `GET /v1/health` answered 200.
    public func isHealthy() async -> Bool {
        guard let (_, status) = try? await send(request("GET", "/v1/health")) else { return false }
        return status == 200
    }

    public func localStatus() async throws -> LocalStatus {
        try await call("GET", "/v1/local/status", as: LocalStatus.self)
    }

    /// `POST /v1/local/session {"client":"mac-app"}`: a token for the machine's owner. One
    /// live token per client kind, so signing in again replaces the app's last one.
    public func localSession(client: String = "mac-app") async throws -> LocalSession {
        try await call("POST", "/v1/local/session", body: Self.json(["client": client]), as: LocalSession.self)
    }

    public func whoami() async throws -> WhoAmI {
        try await call("GET", "/v1/whoami", as: WhoAmI.self)
    }

    public func status(project: String) async throws -> StatusSummary {
        try await call("GET", "/v1/projects/\(Self.segment(project))/status", as: StatusSummary.self)
    }

    public func assignments(session: String) async throws -> [Assignment] {
        try await call("GET", "/v1/sessions/\(Self.segment(session))/assignments", as: AssignmentList.self).assignments
    }

    public func respond(assignment: String, accept: Bool, note: String = "") async throws -> Assignment {
        struct Body: Encodable { var accept: Bool; var note: String }
        return try await call("POST", "/v1/assignments/\(Self.segment(assignment))/respond",
                              body: Self.json(Body(accept: accept, note: note)), as: Assignment.self)
    }

    /// Marks a conflict acknowledged: seen, and being dealt with.
    public func acknowledge(conflict: String, note: String = "") async throws {
        struct Body: Encodable { var state: String; var note: String }
        _ = try await call("POST", "/v1/conflicts/\(Self.segment(conflict))/resolve",
                           body: Self.json(Body(state: "acknowledged", note: note)), as: JSONValue.self)
    }

    /// What `conductor invite` does: add a member and mint their token.
    public func invite(project: String, _ request: InviteRequest) async throws -> InviteResult {
        try await call("POST", "/v1/projects/\(Self.segment(project))/members", body: Self.json(request), as: InviteResult.self)
    }

    /// The event stream's URL for a project.
    public func eventStreamURL(project: String) -> URL {
        request("GET", "/v1/projects/\(Self.segment(project))/events/stream").url!
    }
}
