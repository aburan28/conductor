import Foundation

/// Join links: what Invite hands to Messages, and what `conductor://join` carries.
///
/// The web form is exactly what `conductor invite` prints (cmd/conductor/invite.go joinLink):
/// `<endpoint>/#project=<p>&token=<t>`, form-encoded the way Go's url.Values.Encode does it,
/// keys sorted. The token rides in the fragment, which a browser never sends to the server.
/// The app form is the same fragment with the endpoint added, under `conductor://join`, so
/// a Mac with the app installed opens the app, which runs the join and connects the tools.
public enum InviteLink {
    public static let scheme = "conductor"

    /// The link `conductor join` and the dashboard both accept.
    public static func web(endpoint: String, project: String, token: String) -> String {
        trimSlash(endpoint) + "/#" + formEncode([("project", project), ("token", token)])
    }

    /// `conductor://join#endpoint=…&project=…&token=…`.
    public static func app(endpoint: String, project: String, token: String) -> String {
        "\(scheme)://join#" + formEncode([("endpoint", trimSlash(endpoint)), ("project", project), ("token", token)])
    }

    /// The line handed to the share sheet with the link.
    public static func shareText(_ link: String) -> String {
        "Join my Conductor swarm: \(link)"
    }

    public struct Join: Equatable, Sendable {
        public var endpoint: String
        public var project: String?
        public var token: String

        /// The web form, which is what `conductor join` is given.
        public var webLink: String {
            var pairs = [("token", token)]
            if let project, !project.isEmpty { pairs.insert(("project", project), at: 0) }
            return trimSlash(endpoint) + "/#" + formEncode(pairs)
        }
    }

    public enum ParseError: Error, Equatable, CustomStringConvertible {
        case notAJoinLink
        case noToken
        case noEndpoint
        case badEndpoint(String)

        public var description: String {
            switch self {
            case .notAJoinLink: return "This is not a Conductor join link."
            case .noToken: return "The link carries no token. Ask for the whole link again."
            case .noEndpoint: return "The link does not say which Conductor to join."
            case .badEndpoint(let e): return "The link's address, \(e), is not an http or https URL."
            }
        }
    }

    /// Reads a `conductor://join` link: the values in its fragment (preferred) or query.
    /// Also accepts the web form, so pasting either works.
    public static func parse(_ s: String) -> Result<Join, ParseError> {
        let text = s.trimmingCharacters(in: .whitespacesAndNewlines)
        guard let comps = URLComponents(string: text), let scheme = comps.scheme?.lowercased() else {
            return .failure(.notAJoinLink)
        }
        var values: [String: String] = [:]
        func take(_ encoded: String?) {
            guard let encoded, !encoded.isEmpty else { return }
            for (k, v) in formDecode(encoded) where values[k] == nil { values[k] = v }
        }
        take(comps.percentEncodedFragment)
        take(comps.percentEncodedQuery)

        var endpoint: String
        switch scheme {
        case Self.scheme:
            guard comps.host?.lowercased() == "join" || comps.path.lowercased().hasPrefix("join")
                    || comps.path.lowercased().hasPrefix("//join") else { return .failure(.notAJoinLink) }
            guard let e = values["endpoint"], !e.isEmpty else {
                return values["token"] == nil ? .failure(.noToken) : .failure(.noEndpoint)
            }
            endpoint = e
        case "http", "https":
            guard let host = comps.host else { return .failure(.notAJoinLink) }
            endpoint = "\(scheme)://\(host)" + (comps.port.map { ":\($0)" } ?? "")
        default:
            return .failure(.notAJoinLink)
        }
        guard let token = values["token"], !token.isEmpty else { return .failure(.noToken) }
        endpoint = trimSlash(endpoint)
        guard let u = URL(string: endpoint), let es = u.scheme?.lowercased(), es == "http" || es == "https", u.host != nil else {
            return .failure(.badEndpoint(endpoint))
        }
        return .success(Join(endpoint: endpoint, project: values["project"], token: token))
    }

    /// Whether a join link points only at the machine it was made on, so nobody else can
    /// use it (cmd/conductor/invite.go isLoopbackEndpoint).
    public static func isLoopback(_ endpoint: String) -> Bool {
        guard let host = URL(string: endpoint)?.host?.lowercased() else { return false }
        let bare = host.trimmingCharacters(in: CharacterSet(charactersIn: "[]"))
        return bare == "localhost" || bare == "::1" || bare.hasPrefix("127.")
    }

    // MARK: - encoding

    /// Go's url.QueryEscape: everything but ASCII letters, digits and `-_.~` is %XX
    /// (uppercase), and a space is `+`.
    public static func queryEscape(_ s: String) -> String {
        var out = ""
        for byte in s.utf8 {
            switch byte {
            case UInt8(ascii: "a")...UInt8(ascii: "z"), UInt8(ascii: "A")...UInt8(ascii: "Z"),
                 UInt8(ascii: "0")...UInt8(ascii: "9"),
                 UInt8(ascii: "-"), UInt8(ascii: "_"), UInt8(ascii: "."), UInt8(ascii: "~"):
                out.append(Character(UnicodeScalar(byte)))
            case UInt8(ascii: " "):
                out.append("+")
            default:
                out += String(format: "%%%02X", byte)
            }
        }
        return out
    }

    /// url.Values.Encode: pairs sorted by key, `k=v` joined with `&`.
    public static func formEncode(_ pairs: [(String, String)]) -> String {
        pairs.sorted { $0.0 < $1.0 }
            .map { queryEscape($0.0) + "=" + queryEscape($0.1) }
            .joined(separator: "&")
    }

    /// The inverse: `+` is a space, %XX a byte.
    public static func formDecode(_ s: String) -> [(String, String)] {
        s.split(separator: "&").compactMap { part in
            let kv = part.split(separator: "=", maxSplits: 1, omittingEmptySubsequences: false)
            guard let k = kv.first.map(String.init).flatMap(unescape), !k.isEmpty else { return nil }
            let v = kv.count > 1 ? unescape(String(kv[1])) ?? "" : ""
            return (k, v)
        }
    }

    static func unescape(_ s: String) -> String? {
        s.replacingOccurrences(of: "+", with: " ").removingPercentEncoding
    }

    static func trimSlash(_ s: String) -> String {
        var t = s.trimmingCharacters(in: .whitespaces)
        while t.hasSuffix("/") { t.removeLast() }
        return t
    }
}

/// Token lifetimes Invite offers. Humans are capped at 90 days by the server.
public enum InviteExpiry: String, CaseIterable, Identifiable, Sendable {
    case day = "24h"
    case week = "168h"
    case month = "720h"
    case serverDefault = ""

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .day: return "1 day"
        case .week: return "7 days"
        case .month: return "30 days"
        case .serverDefault: return "90 days (the longest)"
        }
    }

    /// What goes in `token_ttl`, or nil for the server's default.
    public var ttl: String? { rawValue.isEmpty ? nil : rawValue }
}

/// Roles an invite can grant (cmd/conductor/invite.go).
public enum InviteRole: String, CaseIterable, Identifiable, Sendable {
    case contributor, reviewer, maintainer, observer
    case projectAdmin = "project_admin"

    public var id: String { rawValue }
    public var title: String { rawValue.replacingOccurrences(of: "_", with: " ").capitalized }
}
