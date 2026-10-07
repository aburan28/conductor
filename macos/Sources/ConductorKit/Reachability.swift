import Foundation

/// Tailscale, the easy way to let someone across town reach this Mac: `tailscale serve`
/// publishes the loopback daemon to the person's tailnet only, with Tailscale terminating
/// TLS. Requests through the tailnet name are not from localhost, so they need the token in
/// the link; local sign-in never leaks through it.
public enum Tailscale {
    /// Where the command is: the Mac App Store and standalone apps put it inside the app
    /// bundle; Homebrew and the open-source build put it on the PATH.
    public static let candidates = [
        "/Applications/Tailscale.app/Contents/MacOS/Tailscale",
        "/opt/homebrew/bin/tailscale",
        "/usr/local/bin/tailscale",
    ]

    public static func locate(isExecutable: (String) -> Bool = { FileManager.default.isExecutableFile(atPath: $0) }) -> URL? {
        candidates.first(where: isExecutable).map { URL(fileURLWithPath: $0) }
    }

    public static let statusArguments = ["status", "--json"]

    /// `tailscale serve --bg <port>`: run only after the person clicks.
    public static func serveArguments(port: Int) -> [String] { ["serve", "--bg", String(port)] }

    /// How to stop sharing again.
    public static let resetCommand = "tailscale serve reset"

    public static let serveStatusArguments = ["serve", "status", "--json"]

    /// Whether `tailscale serve status --json` shows a handler proxying to this port on
    /// loopback: `{"Web": {"host:443": {"Handlers": {"/": {"Proxy": "http://127.0.0.1:8080"}}}}}`.
    public static func isServing(port: Int, serveStatusJSON: Data) -> Bool {
        guard let web = (try? JSONValue.decode(serveStatusJSON))?["Web"]?.objectValue else { return false }
        let targets = ["127.0.0.1:\(port)", "localhost:\(port)"]
        for site in web.values {
            for handler in (site["Handlers"]?.objectValue ?? [:]).values {
                guard let proxy = handler["Proxy"]?.stringValue else { continue }
                let bare = proxy.replacingOccurrences(of: "http://", with: "").replacingOccurrences(of: "https+insecure://", with: "")
                if targets.contains(where: { bare.hasPrefix($0) }) || bare == String(port) { return true }
            }
        }
        return false
    }

    /// The machine's MagicDNS name when Tailscale is connected (cmd/conductor/invite.go
    /// parseTailscaleStatus): `BackendState` is `Running` and `Self.DNSName` is set.
    public static func magicDNSName(statusJSON: Data) -> String? {
        guard let v = try? JSONValue.decode(statusJSON),
              v["BackendState"]?.stringValue == "Running",
              var name = v["Self"]?["DNSName"]?.stringValue else { return nil }
        while name.hasSuffix(".") { name.removeLast() }
        return name.isEmpty ? nil : name
    }
}

/// Where a teammate could reach this Mac's control plane, best first: an explicit public URL
/// the daemon was started with, then Tailscale, else nowhere (and the sheet says so).
public enum InviteReachability: Equatable, Sendable {
    case publicURL(String)
    case tailscaleAvailable(name: String)
    case tailscaleServing(endpoint: String)
    case localOnly

    public static func decide(publicURL: String?, tailscaleName: String?, tailscaleServing: Bool) -> InviteReachability {
        if let publicURL, !publicURL.trimmingCharacters(in: .whitespaces).isEmpty, !InviteLink.isLoopback(publicURL) {
            return .publicURL(publicURL)
        }
        if let name = tailscaleName {
            return tailscaleServing ? .tailscaleServing(endpoint: "https://\(name)") : .tailscaleAvailable(name: name)
        }
        return .localOnly
    }

    /// The endpoint to put in the link, when there is one someone else can use.
    public var endpoint: String? {
        switch self {
        case .publicURL(let u): return u
        case .tailscaleServing(let e): return e
        case .tailscaleAvailable, .localOnly: return nil
        }
    }

    public var explanation: String {
        switch self {
        case .publicURL(let u):
            return "The link points at \(u), the public address this Conductor was started with."
        case .tailscaleAvailable(let name):
            return "Tailscale is running on this Mac (\(name)). Share over Tailscale to publish Conductor to your tailnet only; nothing becomes reachable from the internet. If they are not on your tailnet, share this machine with them from the Tailscale admin console first."
        case .tailscaleServing(let e):
            return "Conductor is shared on your tailnet at \(e). Stop sharing with `\(Tailscale.resetCommand)`."
        case .localOnly:
            return "This link points at 127.0.0.1, which only reaches this Mac. The other person must be able to reach this Mac: install Tailscale, or set a public URL in Settings."
        }
    }
}
