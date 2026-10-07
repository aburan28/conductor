import Foundation

/// The binaries the app runs: the Go commands it ships in `Contents/Resources/bin`, and the
/// Postgres bundle in `Contents/Resources/postgres/bin`.
public struct ConductorBinaries: Equatable, Sendable {
    public var conductor: URL
    public var conductord: URL
    public var conductorMCP: URL
    /// nil when this build carries no Postgres (built with `build.sh --no-postgres`), in which
    /// case the app can only attach to a database the person runs.
    public var postgresBin: URL?

    public init(conductor: URL, conductord: URL, conductorMCP: URL, postgresBin: URL?) {
        self.conductor = conductor
        self.conductord = conductord
        self.conductorMCP = conductorMCP
        self.postgresBin = postgresBin
    }

    /// Where the binaries are: `$CONDUCTOR_BIN_DIR` when set (a development override), else
    /// the bundle's own `Resources/bin`, else the installer's `/usr/local/bin` symlinks.
    /// Never `$PATH`: an app started from Finder gets a minimal one, and an old
    /// `~/.local/bin/conductor` is the copy most likely to be stale.
    public static func locate(resources: URL?, environment: [String: String],
                              isExecutable: (String) -> Bool = { FileManager.default.isExecutableFile(atPath: $0) }) -> ConductorBinaries? {
        var dirs: [URL] = []
        if let override = environment["CONDUCTOR_BIN_DIR"], !override.isEmpty {
            dirs.append(URL(fileURLWithPath: override, isDirectory: true))
        }
        if let resources { dirs.append(resources.appendingPathComponent("bin", isDirectory: true)) }
        dirs.append(URL(fileURLWithPath: "/usr/local/bin", isDirectory: true))
        guard let dir = dirs.first(where: { isExecutable($0.appendingPathComponent("conductor").path)
                                            && isExecutable($0.appendingPathComponent("conductord").path) })
        else { return nil }
        var pg: URL?
        if let override = environment["CONDUCTOR_POSTGRES_BIN"], !override.isEmpty {
            pg = URL(fileURLWithPath: override, isDirectory: true)
        } else if let resources {
            pg = resources.appendingPathComponent("postgres/bin", isDirectory: true)
        }
        if let candidate = pg, !isExecutable(candidate.appendingPathComponent("postgres").path) {
            pg = nil
        }
        return ConductorBinaries(
            conductor: dir.appendingPathComponent("conductor"),
            conductord: dir.appendingPathComponent("conductord"),
            conductorMCP: dir.appendingPathComponent("conductor-mcp"),
            postgresBin: pg
        )
    }

    /// The environment every command the app starts runs with, launchd agents included.
    ///
    /// Finder gives an app `/usr/bin:/bin:/usr/sbin:/sbin`. The commands need the bundled
    /// binaries first (Postgres runs `conductor db archive-wal` by name; `conductor db
    /// base-backup` runs `pg_basebackup`), then where people install `git`, `gh`, `aws`,
    /// `tailscale` and the coding tools.
    public func environment(base: [String: String], home: URL) -> [String: String] {
        var env = base
        var path: [String] = [conductor.deletingLastPathComponent().path]
        if let postgresBin { path.append(postgresBin.path) }
        path += ["/opt/homebrew/bin", "/usr/local/bin"]
        let existing = (base["PATH"] ?? "").split(separator: ":").map(String.init).filter { !$0.isEmpty }
        for p in existing + ["/usr/bin", "/bin", "/usr/sbin", "/sbin"] where !path.contains(p) {
            path.append(p)
        }
        env["PATH"] = path.joined(separator: ":")
        if env["HOME"] == nil { env["HOME"] = home.path }
        return env
    }

    /// The part of `environment` a launchd agent gets: the PATH, HOME, the CLI's own
    /// settings (`CONDUCTOR_*`), and the AWS variables `storage.json`'s `profile` method
    /// reads. Not the app's whole environment, and never a secret: a plist is a plain file
    /// any process of this user can read, so a variable named like a key, token, secret or
    /// password stays out of it (`CONDUCTOR_CHECKPOINT_KEY` included: the CLI falls back to
    /// the Keychain item dev.conductor.seal/default for the seal passphrase).
    public func agentEnvironment(base: [String: String], home: URL) -> [String: String] {
        let full = environment(base: base, home: home)
        var env: [String: String] = [:]
        for (key, value) in full where !Self.isSecretName(key) {
            if key == "PATH" || key == "HOME" || key == "USER" || key == "LANG"
                || key.hasPrefix("CONDUCTOR_") || key.hasPrefix("AWS_") {
                env[key] = value
            }
        }
        return env
    }

    /// A variable whose name says it holds a secret. One that names a file holding one is a
    /// path, not a secret.
    public static func isSecretName(_ key: String) -> Bool {
        let k = key.uppercased()
        if k.hasSuffix("_FILE") { return false }
        if k == "AWS_ACCESS_KEY_ID" { return true }
        return ["KEY", "TOKEN", "SECRET", "PASSWORD"].contains { k.contains($0) }
    }
}

/// The argument lists for every `conductor` and `conductord` command the app runs, in one
/// place, so a change to the CLI's flags is one change here and a failing test.
public enum ConductorCommands {
    // MARK: storage (docs/STORAGE.md)

    public static let storageShow = ["storage", "show", "--json"]
    public static let storageTest = ["storage", "test", "--json"]
    public static let storageUnset = ["storage", "unset"]
    public static let storageProfiles = ["storage", "profiles", "--json"]

    // MARK: database (docs/STORAGE.md)

    public static func dbArchiving(dataDir: URL) -> [String] {
        ["db", "archiving", "--data-dir", dataDir.path, "--write"]
    }

    /// `conductor db base-backup`, with the bundled Postgres's tools (`pg_basebackup`) named
    /// explicitly rather than found on a PATH. It prunes to `keep_base_backups` afterwards.
    public static func dbBaseBackup(dsn: String, pgBin: URL?) -> [String] {
        var args = ["db", "base-backup", "--dsn", dsn]
        if let pgBin { args += ["--pg-bin", pgBin.path] }
        return args
    }

    public static let dbBackups = ["db", "backups", "--json"]
    public static let dbStatus = ["db", "status", "--json"]
    /// Without the network: only what this machine recorded. Cheap enough to poll.
    public static let dbStatusLocal = ["db", "status", "--json", "--local"]

    public static func dbRestore(dataDir: URL, backup: String = "latest") -> [String] {
        ["db", "restore", "--data-dir", dataDir.path, "--backup", backup]
    }

    // MARK: onboarding

    public static func initRepository(_ dir: URL) -> [String] { ["init", "--dir", dir.path] }

    /// `conductord bootstrap`: the first organization, project and principal, and this
    /// machine's owner, whom local sign-in acts as. The DSN goes in the environment
    /// (`DATABASE_URL`), as `conductor up` passes it.
    public static func bootstrap(repository: URL, endpoint: String) -> [String] {
        ["bootstrap", "--repo", repository.path, "--endpoint", endpoint]
    }

    /// `conductor login` against a loopback endpoint signs in locally, with no token.
    public static func login(endpoint: String) -> [String] { ["login", "--endpoint", endpoint] }

    public static let doctor = ["doctor", "--json"]
    public static let integrateAll = ["integrate", "all", "--global"]
    public static func integrate(_ tool: String) -> [String] { ["integrate", tool, "--global"] }

    /// `conductor join <link>`. `--json` returns before the CLI's own "connect your tools?"
    /// question, so the app connects them afterwards with `integrateAll`.
    public static func join(link: String) -> [String] { ["join", link, "--json", "--no-integrate"] }

    public static let pause = ["pause", "--json"]
    public static let resume = ["resume", "--json"]

    public static let securityStatus = ["security", "status", "--json"]
    public static func setSecurity(_ mode: String) -> [String] { ["security", mode, "--json"] }

    // MARK: portability

    public static let checkpointList = ["checkpoint", "list", "--json"]
    public static let checkpointListAll = ["checkpoint", "list", "--all", "--json"]

    // MARK: GitHub

    public static let githubStatus = ["github", "status", "--json"]
    /// `--no-open`: the app opens the setup page itself, in the person's browser.
    public static func githubSetup(org: String? = nil, replace: Bool = false) -> [String] {
        var args = ["github", "setup", "--json", "--no-open"]
        if let org, !org.isEmpty { args += ["--org", org] }
        if replace { args.append("--replace") }
        return args
    }

    /// `conductor github link [owner/repo]`, run inside the repository's checkout so the CLI
    /// can read its origin when no name is given.
    public static func githubLink(repository: String? = nil, project: String? = nil) -> [String] {
        var args = ["github", "link"]
        if let repository, !repository.isEmpty { args.append(repository) }
        if let project, !project.isEmpty { args += ["--project", project] }
        args.append("--json")
        return args
    }
}
