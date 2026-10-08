import Foundation

/// `conductor doctor --json` (cmd/conductor/doctor.go doctorReport), as much as the Connect
/// tools sheet shows.
public struct DoctorReport: Decodable, Equatable, Sendable {
    /// One coding tool (internal/integrations.Status).
    public struct Integration: Decodable, Equatable, Identifiable, Sendable {
        public var tool: String
        public var title: String
        public var detected: Bool
        public var configured: Bool
        public var transport: String?
        public var configPath: String?
        public var hooksSupported: Bool
        public var hooks: Bool
        public var fix: String?

        public var id: String { tool }

        enum CodingKeys: String, CodingKey {
            case tool, title, detected, configured, transport
            case configPath = "config_path"
            case hooksSupported = "hooks_supported"
            case hooks, fix
        }

        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            tool = try c.decode(String.self, forKey: .tool)
            title = try c.decodeIfPresent(String.self, forKey: .title) ?? tool
            detected = try c.decodeIfPresent(Bool.self, forKey: .detected) ?? false
            configured = try c.decodeIfPresent(Bool.self, forKey: .configured) ?? false
            transport = try c.decodeIfPresent(String.self, forKey: .transport)
            configPath = try c.decodeIfPresent(String.self, forKey: .configPath)
            hooksSupported = try c.decodeIfPresent(Bool.self, forKey: .hooksSupported) ?? false
            hooks = try c.decodeIfPresent(Bool.self, forKey: .hooks) ?? false
            fix = try c.decodeIfPresent(String.self, forKey: .fix)
        }

        /// What the sheet says about the tool.
        public var state: State {
            if !detected { return .notInstalled }
            if !configured { return .notConnected }
            if hooksSupported && !hooks { return .connectedWithoutHooks }
            return .connected
        }

        public enum State: Equatable, Sendable {
            case notInstalled, notConnected, connectedWithoutHooks, connected

            public var label: String {
                switch self {
                case .notInstalled: return "Not installed"
                case .notConnected: return "Not connected"
                case .connectedWithoutHooks: return "Connected, hooks off"
                case .connected: return "Connected"
                }
            }
        }
    }

    public var endpoint: String?
    public var reachable: Bool
    public var principal: String?
    public var project: String?
    public var clientVersion: String?
    public var serverVersion: String?
    public var versionWarning: String?
    public var databaseOK: Bool
    public var integrations: [Integration]

    enum CodingKeys: String, CodingKey {
        case endpoint, reachable, principal, project
        case clientVersion = "client_version"
        case serverVersion = "server_version"
        case versionWarning = "version_warning"
        case databaseOK = "database_ok"
        case integrations
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        endpoint = try c.decodeIfPresent(String.self, forKey: .endpoint)
        reachable = try c.decodeIfPresent(Bool.self, forKey: .reachable) ?? false
        principal = try c.decodeIfPresent(String.self, forKey: .principal)
        project = try c.decodeIfPresent(String.self, forKey: .project)
        clientVersion = try c.decodeIfPresent(String.self, forKey: .clientVersion)
        serverVersion = try c.decodeIfPresent(String.self, forKey: .serverVersion)
        versionWarning = try c.decodeIfPresent(String.self, forKey: .versionWarning)
        databaseOK = try c.decodeIfPresent(Bool.self, forKey: .databaseOK) ?? false
        integrations = try c.decodeIfPresent([Integration].self, forKey: .integrations) ?? []
    }

    public static func decode(_ data: Data) throws -> DoctorReport {
        try JSONDecoder().decode(DoctorReport.self, from: data)
    }

    /// Tools that are installed but not yet connected: what "Connect all" would change.
    public var toolsToConnect: [Integration] {
        integrations.filter { $0.detected && $0.state != .connected }
    }

    /// At least one installed tool is connected.
    public var anyToolConnected: Bool {
        integrations.contains { $0.detected && $0.configured }
    }
}

/// `conductor github status --json` (cmd/conductor/github.go githubStatusView).
public struct GitHubStatus: Decodable, Equatable, Sendable {
    public struct App: Decodable, Equatable, Sendable {
        public var id: Int?
        public var slug: String?
        public var name: String?
        public var owner: String?
        public var htmlURL: String?

        enum CodingKeys: String, CodingKey {
            case id, slug, name, owner
            case htmlURL = "html_url"
        }
    }

    public struct Installation: Decodable, Equatable, Identifiable, Sendable {
        public var id: Int
        public var account: String
        public var targetType: String?
        public var htmlURL: String?

        enum CodingKeys: String, CodingKey {
            case id, account
            case targetType = "target_type"
            case htmlURL = "html_url"
        }
    }

    public struct PermissionGap: Decodable, Equatable, Sendable {
        public var scope: String?
        public var missing: String?
        public var fix: String?
        public var url: String?
    }

    public var configured: Bool
    public var mode: String?
    public var app: App?
    public var installURL: String?
    public var installations: [Installation]
    public var installationsError: String?
    public var permissionGaps: [PermissionGap]
    /// `{"project", "repository", "issues"}` per linked project.
    public var linked: [[String: String]]
    public var lastPoll: String?
    public var lastError: String?

    enum CodingKeys: String, CodingKey {
        case configured, mode, app
        case installURL = "install_url"
        case installations
        case installationsError = "installations_error"
        case permissionGaps = "permission_gaps"
        case linked
        case lastPoll = "last_poll"
        case lastError = "last_error"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        configured = try c.decodeIfPresent(Bool.self, forKey: .configured) ?? false
        mode = try c.decodeIfPresent(String.self, forKey: .mode)
        app = try c.decodeIfPresent(App.self, forKey: .app)
        installURL = try c.decodeIfPresent(String.self, forKey: .installURL)
        installations = try c.decodeIfPresent([Installation].self, forKey: .installations) ?? []
        installationsError = try c.decodeIfPresent(String.self, forKey: .installationsError)
        permissionGaps = try c.decodeIfPresent([PermissionGap].self, forKey: .permissionGaps) ?? []
        linked = try c.decodeIfPresent([[String: String]].self, forKey: .linked) ?? []
        lastPoll = try c.decodeIfPresent(String.self, forKey: .lastPoll)
        lastError = try c.decodeIfPresent(String.self, forKey: .lastError)
    }

    public static func decode(_ data: Data) throws -> GitHubStatus {
        try JSONDecoder().decode(GitHubStatus.self, from: data)
    }

    /// Where the GitHub sheet is: no app yet, an app nobody installed, or working.
    public enum Stage: Equatable, Sendable {
        case noApp, notInstalled, installed
    }

    public var stage: Stage {
        guard configured, app != nil else { return .noApp }
        return installations.isEmpty ? .notInstalled : .installed
    }
}

/// `conductor github setup --json`: the one-time page that creates the app on GitHub.
public struct GitHubSetup: Decodable, Equatable, Sendable {
    public var setupURL: String
    public var expiresAt: String?
    public var name: String?
    /// False when this Conductor is not reachable from the internet and polls instead.
    public var webhooks: Bool?

    enum CodingKeys: String, CodingKey {
        case setupURL = "setup_url"
        case expiresAt = "expires_at"
        case name, webhooks
    }
}

/// `conductor join --json`.
public struct JoinResult: Decodable, Equatable, Sendable {
    public var endpoint: String?
    public var handle: String?
    public var project: String?
}

/// `conductor security status --json`.
public struct SecurityStatus: Decodable, Equatable, Sendable {
    public var securityMode: String
    public var modeSource: String?
    public var owner: String?
    public var youAreOwner: Bool?
    public var behindProxy: Bool?

    enum CodingKeys: String, CodingKey {
        case securityMode = "security_mode"
        case modeSource = "mode_source"
        case owner
        case youAreOwner = "you_are_owner"
        case behindProxy = "behind_proxy"
    }

    /// The mode cannot be changed here: conductord was started with --security-mode.
    public var isPinned: Bool { modeSource == "flag" }
}
