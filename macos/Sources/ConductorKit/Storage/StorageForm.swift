import Foundation

/// What the Settings → Storage pane edits, and the one place that turns it into a `conductor
/// storage set` command line.
///
/// `storage set` merges into the file: only the flags given change. The pane gives every flag
/// it shows, so what is saved is exactly what is on screen, whatever the file held before.
/// Every flag is written `--name=value`: the CLI rejects a boolean written with a space
/// (`--sessions false`), and `=` also keeps an empty value (clearing the endpoint) and a
/// value that starts with a dash unambiguous.
public struct StorageForm: Equatable, Sendable {
    public var bucket = ""
    public var region = ""
    public var prefix = ""
    public var endpoint = ""
    public var pathStyle = false
    public var insecure = false

    public var sessions = true
    public var checkpoints = true
    public var database = true

    public var method: StorageAuthMethod = .static
    public var accessKeyID = ""
    /// A secret typed into the pane and not yet saved. Held only in memory: it goes to the
    /// Keychain on Save and is cleared.
    public var newSecret = ""
    public var profile = ""
    /// A seal passphrase typed into the pane and not yet saved; memory only, like `newSecret`.
    public var newSealPassphrase = ""

    public var archiveWAL = true
    public var archiveTimeoutSeconds = StorageSettings.Database.defaultArchiveTimeout
    public var baseBackupEveryHours = StorageSettings.Database.defaultBaseBackupEveryHours
    public var keepBaseBackups = StorageSettings.Database.defaultKeepBaseBackups
    public var seal = true

    public init() {}

    /// The pane as `storage show --json` describes the saved settings.
    public init(_ show: StorageShow) {
        self.init(show.settings, configured: show.configured)
    }

    public init(_ s: StorageSettings, configured: Bool = true) {
        bucket = s.s3.bucket
        region = s.s3.region
        prefix = s.s3.prefix
        endpoint = s.s3.endpoint
        pathStyle = s.s3.pathStyle
        insecure = s.s3.insecure
        sessions = s.uses.sessions
        checkpoints = s.uses.checkpoints
        database = s.uses.database
        // A fresh pane offers an access key first; a saved one shows what was saved.
        method = configured ? s.auth.method : .static
        accessKeyID = s.auth.accessKeyID
        profile = s.auth.profile
        archiveWAL = s.database.archiveWAL
        archiveTimeoutSeconds = s.database.archiveTimeoutSeconds
        baseBackupEveryHours = s.database.baseBackupEveryHours
        keepBaseBackups = s.database.keepBaseBackups
        seal = s.database.seal
    }

    /// Whether the S3-compatible endpoint section holds anything, so the pane opens it.
    public var usesCustomEndpoint: Bool {
        !endpoint.trimmingCharacters(in: .whitespaces).isEmpty || pathStyle || insecure
    }

    public var trimmedAccessKeyID: String { accessKeyID.trimmingCharacters(in: .whitespacesAndNewlines) }
    public var trimmedSecret: String { newSecret.trimmingCharacters(in: .whitespacesAndNewlines) }

    /// What stops a save, in words for the pane. `secretInKeychain` says whether the Keychain
    /// already holds a secret for this access key ID; `sealPassphraseSet`, whether it holds the
    /// seal passphrase (or CONDUCTOR_CHECKPOINT_KEY provides one).
    public func problems(secretInKeychain: Bool, sealPassphraseSet: Bool = true) -> [String] {
        var out: [String] = []
        if bucket.trimmingCharacters(in: .whitespaces).isEmpty {
            out.append("Enter the bucket name.")
        }
        let ep = endpoint.trimmingCharacters(in: .whitespaces)
        if !ep.isEmpty {
            let lower = ep.lowercased()
            if !(lower.hasPrefix("https://") || lower.hasPrefix("http://")) || URL(string: ep)?.host == nil {
                out.append("The endpoint must be a URL, such as https://minio.example.com:9000.")
            } else if lower.hasPrefix("http://") && !insecure {
                out.append("The endpoint is plain http. Turn on \"Allow plain HTTP\" or use https.")
            }
        }
        switch method {
        case .static:
            if trimmedAccessKeyID.isEmpty {
                out.append("Enter the access key ID.")
            }
            if trimmedSecret.isEmpty && !secretInKeychain {
                out.append("Enter the secret access key.")
            }
            if newSecret.contains("\n") || newSecret.contains("\r") {
                out.append("The secret access key cannot contain a line break.")
            }
        case .profile, .environment:
            break
        }
        if database {
            if archiveTimeoutSeconds < 1 { out.append("Archive at least every 1 second or more.") }
            if baseBackupEveryHours < 1 { out.append("Take a base backup at least every hour.") }
            if keepBaseBackups < 1 { out.append("Keep at least one base backup.") }
            if seal && !sealPassphraseSet && newSealPassphrase.isEmpty {
                out.append("Set a seal passphrase, or turn sealing off.")
            }
        }
        return out
    }

    /// Where `storage set` finds the secret of an access key.
    public enum SecretSource: Equatable, Sendable {
        /// Already in the Keychain under dev.conductor.s3 / the access key ID (the app wrote
        /// it); the CLI reads it back to confirm.
        case keychain
        /// On standard input; the CLI stores it in the Keychain (`--secret-store=keychain`,
        /// never the file).
        case stdin
    }

    /// The arguments to `conductor` for Save, after `storage set`. Nothing secret is in them:
    /// with `.stdin` the caller writes the secret to the process's standard input.
    public func setArguments(secret: SecretSource) -> [String] {
        var args = ["storage", "set", "--json"]
        func flag(_ name: String, _ value: String) { args.append("--\(name)=\(value)") }
        func flag(_ name: String, _ value: Bool) { args.append("--\(name)=\(value ? "true" : "false")") }
        func flag(_ name: String, _ value: Int) { args.append("--\(name)=\(value)") }

        flag("bucket", bucket.trimmingCharacters(in: .whitespaces))
        flag("region", region.trimmingCharacters(in: .whitespaces))
        flag("prefix", prefix.trimmingCharacters(in: .whitespaces))
        flag("endpoint", endpoint.trimmingCharacters(in: .whitespaces))
        flag("path-style", pathStyle)
        flag("insecure", insecure)

        flag("sessions", sessions)
        flag("checkpoints", checkpoints)
        flag("database", database)
        flag("archive-wal", archiveWAL)
        flag("archive-timeout", archiveTimeoutSeconds)
        flag("base-backup-every", baseBackupEveryHours)
        flag("keep", keepBaseBackups)
        flag("seal", seal)

        flag("auth", method.rawValue)
        switch method {
        case .static:
            flag("access-key-id", trimmedAccessKeyID)
            switch secret {
            case .keychain:
                flag("secret-from", "keychain")
            case .stdin:
                flag("secret-from", "stdin")
                flag("secret-store", "keychain")
            }
        case .profile:
            flag("profile", profile.trimmingCharacters(in: .whitespaces))
        case .environment:
            break
        }
        return args
    }

    /// What goes on standard input with `.stdin`: the secret and a newline, which the CLI
    /// reads as one line.
    public var secretStdin: Data { Data((trimmedSecret + "\n").utf8) }
}
