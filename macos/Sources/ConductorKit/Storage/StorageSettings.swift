import Foundation

// The bucket settings of docs/STORAGE.md: `storage.json`, what `conductor storage show
// --json` prints, and what the Settings → Storage pane edits. The CLI owns the file; the app
// reads it through `storage show --json` and changes it through `storage set`, which merges.

public enum StorageAuthMethod: String, Codable, CaseIterable, Identifiable, Sendable {
    case `static`
    case profile
    case environment

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .static: return "Access key"
        case .profile: return "AWS profile"
        case .environment: return "Environment / instance role"
        }
    }
}

/// Where a static secret is kept (`auth.secret`).
public enum StorageSecretStore: String, Codable, Sendable {
    case keychain
    case file
}

/// `storage.json`, field for field. Fields the file leaves out decode to the CLI's effective
/// defaults (internal/storage: every use and database switch on, 60 s, 24 h, keep 7).
public struct StorageSettings: Codable, Equatable, Sendable {
    public struct S3: Codable, Equatable, Sendable {
        public var bucket: String
        public var region: String
        public var endpoint: String
        public var pathStyle: Bool
        public var insecure: Bool
        public var prefix: String

        enum CodingKeys: String, CodingKey {
            case bucket, region, endpoint
            case pathStyle = "path_style"
            case insecure, prefix
        }

        public init(bucket: String = "", region: String = "", endpoint: String = "",
                    pathStyle: Bool = false, insecure: Bool = false, prefix: String = "") {
            self.bucket = bucket
            self.region = region
            self.endpoint = endpoint
            self.pathStyle = pathStyle
            self.insecure = insecure
            self.prefix = prefix
        }

        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            bucket = try c.decodeIfPresent(String.self, forKey: .bucket) ?? ""
            region = try c.decodeIfPresent(String.self, forKey: .region) ?? ""
            endpoint = try c.decodeIfPresent(String.self, forKey: .endpoint) ?? ""
            pathStyle = try c.decodeIfPresent(Bool.self, forKey: .pathStyle) ?? false
            insecure = try c.decodeIfPresent(Bool.self, forKey: .insecure) ?? false
            prefix = try c.decodeIfPresent(String.self, forKey: .prefix) ?? ""
        }
    }

    public struct Auth: Codable, Equatable, Sendable {
        public var method: StorageAuthMethod
        public var accessKeyID: String
        /// `keychain`, `file`, or empty when the method is not `static`.
        public var secret: String
        /// Only for `secret: "file"`, which the app never chooses; read so a round trip of the
        /// file is faithful, never shown and never written by the app.
        public var secretAccessKey: String
        public var profile: String

        enum CodingKeys: String, CodingKey {
            case method
            case accessKeyID = "access_key_id"
            case secret
            case secretAccessKey = "secret_access_key"
            case profile
        }

        public init(method: StorageAuthMethod = .environment, accessKeyID: String = "", secret: String = "",
                    secretAccessKey: String = "", profile: String = "") {
            self.method = method
            self.accessKeyID = accessKeyID
            self.secret = secret
            self.secretAccessKey = secretAccessKey
            self.profile = profile
        }

        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            let raw = try c.decodeIfPresent(String.self, forKey: .method) ?? ""
            method = StorageAuthMethod(rawValue: raw) ?? .environment
            accessKeyID = try c.decodeIfPresent(String.self, forKey: .accessKeyID) ?? ""
            secret = try c.decodeIfPresent(String.self, forKey: .secret) ?? ""
            secretAccessKey = try c.decodeIfPresent(String.self, forKey: .secretAccessKey) ?? ""
            profile = try c.decodeIfPresent(String.self, forKey: .profile) ?? ""
        }
    }

    public struct Uses: Codable, Equatable, Sendable {
        public var sessions: Bool
        public var checkpoints: Bool
        public var database: Bool

        public init(sessions: Bool = true, checkpoints: Bool = true, database: Bool = true) {
            self.sessions = sessions
            self.checkpoints = checkpoints
            self.database = database
        }

        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            sessions = try c.decodeIfPresent(Bool.self, forKey: .sessions) ?? true
            checkpoints = try c.decodeIfPresent(Bool.self, forKey: .checkpoints) ?? true
            database = try c.decodeIfPresent(Bool.self, forKey: .database) ?? true
        }
    }

    public struct Database: Codable, Equatable, Sendable {
        public static let defaultArchiveTimeout = 60
        public static let defaultBaseBackupEveryHours = 24
        public static let defaultKeepBaseBackups = 7

        public var archiveWAL: Bool
        public var archiveTimeoutSeconds: Int
        public var baseBackupEveryHours: Int
        public var keepBaseBackups: Int
        public var seal: Bool

        enum CodingKeys: String, CodingKey {
            case archiveWAL = "archive_wal"
            case archiveTimeoutSeconds = "archive_timeout_seconds"
            case baseBackupEveryHours = "base_backup_every_hours"
            case keepBaseBackups = "keep_base_backups"
            case seal
        }

        public init(archiveWAL: Bool = true, archiveTimeoutSeconds: Int = Database.defaultArchiveTimeout,
                    baseBackupEveryHours: Int = Database.defaultBaseBackupEveryHours,
                    keepBaseBackups: Int = Database.defaultKeepBaseBackups, seal: Bool = true) {
            self.archiveWAL = archiveWAL
            self.archiveTimeoutSeconds = archiveTimeoutSeconds
            self.baseBackupEveryHours = baseBackupEveryHours
            self.keepBaseBackups = keepBaseBackups
            self.seal = seal
        }

        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            archiveWAL = try c.decodeIfPresent(Bool.self, forKey: .archiveWAL) ?? true
            seal = try c.decodeIfPresent(Bool.self, forKey: .seal) ?? true
            // Zero or absent means the default, as the CLI reads it.
            let timeout = try c.decodeIfPresent(Int.self, forKey: .archiveTimeoutSeconds) ?? 0
            archiveTimeoutSeconds = timeout > 0 ? timeout : Self.defaultArchiveTimeout
            let every = try c.decodeIfPresent(Int.self, forKey: .baseBackupEveryHours) ?? 0
            baseBackupEveryHours = every > 0 ? every : Self.defaultBaseBackupEveryHours
            let keep = try c.decodeIfPresent(Int.self, forKey: .keepBaseBackups) ?? 0
            keepBaseBackups = keep > 0 ? keep : Self.defaultKeepBaseBackups
        }
    }

    public var version: Int
    public var s3: S3
    public var auth: Auth
    public var uses: Uses
    public var database: Database

    public init(version: Int = 1, s3: S3 = S3(), auth: Auth = Auth(), uses: Uses = Uses(), database: Database = Database()) {
        self.version = version
        self.s3 = s3
        self.auth = auth
        self.uses = uses
        self.database = database
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        version = try c.decodeIfPresent(Int.self, forKey: .version) ?? 1
        s3 = try c.decodeIfPresent(S3.self, forKey: .s3) ?? S3()
        auth = try c.decodeIfPresent(Auth.self, forKey: .auth) ?? Auth()
        uses = try c.decodeIfPresent(Uses.self, forKey: .uses) ?? Uses()
        database = try c.decodeIfPresent(Database.self, forKey: .database) ?? Database()
    }

    enum CodingKeys: String, CodingKey {
        case version, s3, auth, uses, database
    }

    public static func decode(_ data: Data) throws -> StorageSettings {
        try JSONDecoder().decode(StorageSettings.self, from: data)
    }

    /// The prefix every key is under: the configured one, else `conductor`.
    public var effectivePrefix: String {
        let p = s3.prefix.trimmingCharacters(in: CharacterSet(charactersIn: "/ "))
        return p.isEmpty ? "conductor" : p
    }
}

/// `conductor storage show --json`.
public struct StorageShow: Decodable, Equatable, Sendable {
    public var configured: Bool
    public var off: Bool
    public var path: String
    /// `file`, `env` (the CONDUCTOR_BACKUP_S3_* variables override the file), or `none`.
    public var source: String
    public var settings: StorageSettings
    public var effectiveRegion: String?
    public var authDescription: String?

    enum CodingKeys: String, CodingKey {
        case configured, off, path, source, s3, auth, uses, database
        case effectiveRegion = "effective_region"
        case authDescription = "auth_description"
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        configured = try c.decodeIfPresent(Bool.self, forKey: .configured) ?? false
        off = try c.decodeIfPresent(Bool.self, forKey: .off) ?? false
        path = try c.decodeIfPresent(String.self, forKey: .path) ?? ""
        source = try c.decodeIfPresent(String.self, forKey: .source) ?? "none"
        settings = StorageSettings(
            s3: try c.decodeIfPresent(StorageSettings.S3.self, forKey: .s3) ?? .init(),
            auth: try c.decodeIfPresent(StorageSettings.Auth.self, forKey: .auth) ?? .init(),
            uses: try c.decodeIfPresent(StorageSettings.Uses.self, forKey: .uses) ?? .init(),
            database: try c.decodeIfPresent(StorageSettings.Database.self, forKey: .database) ?? .init()
        )
        effectiveRegion = try c.decodeIfPresent(String.self, forKey: .effectiveRegion)
        authDescription = try c.decodeIfPresent(String.self, forKey: .authDescription)
    }

    public static func decode(_ data: Data) throws -> StorageShow {
        try JSONDecoder().decode(StorageShow.self, from: data)
    }

    /// The environment overrides the file, so what the pane would save is not what runs.
    public var overriddenByEnvironment: Bool { source == "env" }

    /// Whether the database is meant to go to the bucket: a bucket is set, uploads are not
    /// turned off, and the database use is on.
    public var databaseToBucket: Bool { configured && !off && settings.uses.database }
}

/// `conductor storage test --json`. The command exits non-zero when `ok` is false and still
/// prints this, so it is decoded from stdout whatever the exit status.
public struct StorageTestResult: Decodable, Equatable, Sendable {
    public struct Step: Decodable, Equatable, Identifiable, Sendable {
        public var name: String
        public var ok: Bool
        public var ms: Int
        public var error: String?

        public var id: String { name }

        public init(name: String, ok: Bool, ms: Int, error: String? = nil) {
            self.name = name
            self.ok = ok
            self.ms = ms
            self.error = error
        }

        enum CodingKeys: String, CodingKey { case name, ok, ms, error }

        public init(from decoder: Decoder) throws {
            let c = try decoder.container(keyedBy: CodingKeys.self)
            name = try c.decodeIfPresent(String.self, forKey: .name) ?? "step"
            ok = try c.decodeIfPresent(Bool.self, forKey: .ok) ?? false
            ms = try c.decodeIfPresent(Int.self, forKey: .ms) ?? 0
            let e = try c.decodeIfPresent(String.self, forKey: .error)
            error = (e?.isEmpty ?? true) ? nil : e
        }
    }

    public var ok: Bool
    public var credentials: String
    public var location: String
    public var steps: [Step]
    public var error: String?

    enum CodingKeys: String, CodingKey { case ok, credentials, location, steps, error }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        ok = try c.decodeIfPresent(Bool.self, forKey: .ok) ?? false
        credentials = try c.decodeIfPresent(String.self, forKey: .credentials) ?? ""
        location = try c.decodeIfPresent(String.self, forKey: .location) ?? ""
        steps = try c.decodeIfPresent([Step].self, forKey: .steps) ?? []
        let e = try c.decodeIfPresent(String.self, forKey: .error)
        error = (e?.isEmpty ?? true) ? nil : e
    }

    /// The result of a `storage test` run: decoded from stdout when it holds the JSON, else a
    /// failure carrying whatever the command said.
    public static func from(_ result: CommandResult) -> Result<StorageTestResult, StorageCommandError> {
        if let decoded = try? JSONDecoder().decode(StorageTestResult.self, from: result.stdout) {
            return .success(decoded)
        }
        return .failure(.command(result.failureMessage("conductor storage test")))
    }
}

/// `conductor storage profiles --json`: `[{"name", "kind", "region"}]`, kind one of static,
/// sso, assume-role, process, unknown.
public struct CLIProfile: Decodable, Equatable, Sendable {
    public var name: String
    public var kind: String
    public var region: String?

    public static func decodeList(_ data: Data) -> [CLIProfile]? {
        try? JSONDecoder().decode([CLIProfile].self, from: data)
    }
}

public enum StorageCommandError: Error, Equatable, CustomStringConvertible {
    case command(String)
    case invalid([String])

    public var description: String {
        switch self {
        case .command(let s): return s
        case .invalid(let problems): return problems.joined(separator: "\n")
        }
    }
}
