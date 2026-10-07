import Foundation
#if canImport(Darwin)
import Darwin
#elseif canImport(Glibc)
import Glibc
#elseif canImport(Musl)
import Musl
#endif

public enum SupervisorError: Error, Equatable, CustomStringConvertible {
    case noBundledPostgres
    case directory(String)
    case initdb(String)
    case restore(String)
    case archiving(String)
    case createDatabase(String)
    case launchctl(String, String)
    case timeout(String)
    case portTaken(Int)
    case bootstrap(String)

    public var description: String {
        switch self {
        case .noBundledPostgres:
            return "This build of Conductor.app carries no Postgres. Attach to a database you run in Settings, or install a build that includes one."
        case .directory(let s): return s
        case .initdb(let s): return "Could not create the database: \(s)"
        case .restore(let s): return "Could not restore the database from the bucket: \(s)"
        case .archiving(let s): return "Could not turn on archiving to the bucket: \(s)"
        case .createDatabase(let s): return "Could not create the conductor database: \(s)"
        case .launchctl(let what, let s): return "launchctl \(what) failed: \(s)"
        case .timeout(let s): return s
        case .portTaken(let port):
            return "Something else is listening on 127.0.0.1:\(port). Choose another port in Settings, or attach to that control plane instead."
        case .bootstrap(let s): return "conductord bootstrap failed: \(s)"
        }
    }
}

/// Where the supervisor is, for the first-run screen and the menu.
public enum SupervisorPhase: Equatable, Sendable {
    case idle
    case preparing
    case initializing
    case restoring
    case configuring
    case startingDatabase
    case startingDaemon
    case running
    case stopped
    case failed(String)

    public var label: String {
        switch self {
        case .idle: return "Not started"
        case .preparing: return "Preparing folders…"
        case .initializing: return "Creating the database…"
        case .restoring: return "Restoring the database from the bucket…"
        case .configuring: return "Configuring the database…"
        case .startingDatabase: return "Starting the database…"
        case .startingDaemon: return "Starting the control plane…"
        case .running: return "Running"
        case .stopped: return "Stopped"
        case .failed(let s): return s
        }
    }
}

/// Everything the supervisor needs to know, fixed for one start.
public struct SupervisorConfig: Sendable {
    public var paths: AppPaths
    public var binaries: ConductorBinaries
    public var daemonPort: Int
    public var publicURL: String
    public var startAtLogin: Bool
    public var uid: UInt32
    public var baseEnvironment: [String: String]
    /// A database the person runs, instead of the bundled one.
    public var externalDSN: String?

    public init(paths: AppPaths, binaries: ConductorBinaries, settings: AppSettings, uid: UInt32,
                baseEnvironment: [String: String], externalDSN: String? = nil) {
        self.paths = paths
        self.binaries = binaries
        self.daemonPort = settings.daemonPort
        self.publicURL = settings.publicURL
        self.startAtLogin = settings.startAtLogin
        self.uid = uid
        self.baseEnvironment = baseEnvironment
        self.externalDSN = externalDSN
    }

    /// The bundled cluster, unless the person attached their own database.
    public var postgres: PostgresSetup? {
        guard externalDSN == nil, let bin = binaries.postgresBin else { return nil }
        return PostgresSetup(paths: paths, binDirectory: bin)
    }

    public var dsn: String? { externalDSN ?? postgres?.dsn }
    public var endpoint: String { "http://127.0.0.1:\(daemonPort)" }

    /// For commands the app runs itself.
    public var commandEnvironment: [String: String] {
        binaries.environment(base: baseEnvironment, home: paths.home)
    }

    /// For the launchd agents.
    public var agentEnvironment: [String: String] {
        binaries.agentEnvironment(base: baseEnvironment, home: paths.home)
    }
}

/// How the supervisor waits. Injected so tests do not wait at all.
public struct SupervisorProbes: Sendable {
    public var daemonHealthy: @Sendable (String) async -> Bool
    public var portFree: @Sendable (Int) -> Bool
    public var sleep: @Sendable (Double) async -> Void
    public var postgresTimeout: Double
    public var daemonTimeout: Double

    public init(daemonHealthy: @escaping @Sendable (String) async -> Bool,
                portFree: @escaping @Sendable (Int) -> Bool = { PortProbe.isFree($0) },
                sleep: @escaping @Sendable (Double) async -> Void = { s in try? await Task.sleep(nanoseconds: UInt64(s * 1_000_000_000)) },
                postgresTimeout: Double = 60, daemonTimeout: Double = 90) {
        self.daemonHealthy = daemonHealthy
        self.portFree = portFree
        self.sleep = sleep
        self.postgresTimeout = postgresTimeout
        self.daemonTimeout = daemonTimeout
    }

    public static let live = SupervisorProbes(daemonHealthy: { endpoint in
        guard let url = URL(string: endpoint) else { return false }
        return await APIClient(endpoint: url).isHealthy()
    })
}

/// Owns the launchd agents that run the private Postgres and conductord, and the commands
/// that set them up: `initdb` (or `conductor db restore`), the configuration the app owns,
/// archiving to the bucket, and the periodic base backup.
///
/// Every step is idempotent: `ensureRunning` at each launch rewrites what the app owns,
/// restarts only what changed, and starts only what is not running, so opening the app over
/// agents launchd already started at login disturbs nothing.
public actor Supervisor {
    public let config: SupervisorConfig
    private let runner: CommandRunning
    private let launchctl: Launchctl
    private let probes: SupervisorProbes
    public private(set) var phase: SupervisorPhase = .idle

    public init(config: SupervisorConfig, runner: CommandRunning = ProcessRunner(), probes: SupervisorProbes = .live) {
        self.config = config
        self.runner = runner
        self.probes = probes
        self.launchctl = Launchctl(uid: config.uid, runner: runner)
    }

    private var fm: FileManager { FileManager.default }

    // MARK: - the whole start

    /// Brings everything up. `restoreFromBucket` is the person's answer to "Restore from
    /// bucket" on a first run; it is ignored once a cluster exists.
    public func ensureRunning(storage: StorageShow?, restoreFromBucket: Bool = false,
                              onPhase: @escaping @Sendable (SupervisorPhase) -> Void = { _ in }) async throws {
        func set(_ p: SupervisorPhase) {
            phase = p
            onPhase(p)
        }
        do {
            set(.preparing)
            try prepareDirectories()
            var postgresChanged = false
            if let pg = config.postgres {
                if !pg.isInitialized() {
                    if restoreFromBucket {
                        set(.restoring)
                        try await restoreCluster()
                    } else {
                        set(.initializing)
                        try await initializeCluster()
                    }
                }
                set(.configuring)
                postgresChanged = try await configureCluster(storage: storage)
                set(.startingDatabase)
                try await startPostgres(restart: postgresChanged)
            } else if config.externalDSN == nil {
                throw SupervisorError.noBundledPostgres
            }
            set(.startingDaemon)
            try await startDaemon()
            try await configureBackups(storage: storage)
            set(.running)
        } catch {
            set(.failed(String(describing: error)))
            throw error
        }
    }

    /// Stops the daemon and the database, leaving their plists for the next start.
    public func stop() async {
        try? await launchctl.bootout(label: LaunchAgents.daemonLabel)
        if config.postgres != nil { await stopPostgres() }
        phase = .stopped
    }

    /// Stops everything and removes the plists: what "Quit and stop Conductor" does when the
    /// person does not want it at login any more. The data stays.
    public func uninstallAgents() async {
        await stop()
        for label in LaunchAgents.allLabels {
            try? await launchctl.bootout(label: label)
            try? fm.removeItem(at: config.paths.launchAgentPlist(label))
        }
    }

    // MARK: - folders

    public func prepareDirectories() throws {
        let p = config.paths
        do {
            try fm.createDirectory(at: p.support, withIntermediateDirectories: true,
                                   attributes: [.posixPermissions: 0o700])
            try fm.createDirectory(at: p.logs, withIntermediateDirectories: true)
            try fm.createDirectory(at: p.launchAgents, withIntermediateDirectories: true)
        } catch {
            throw SupervisorError.directory("Could not create \(p.support.path): \(error.localizedDescription)")
        }
        if config.postgres != nil {
            try Self.preparePrivateDirectory(p.socketDirectory, uid: config.uid)
        }
    }

    /// The socket folder must be this user's alone: its permissions are what keeps other
    /// accounts off a database that trusts every local connection. A folder someone else
    /// created first (possible for the /tmp fallback) is refused, not used.
    public static func preparePrivateDirectory(_ dir: URL, uid: UInt32) throws {
        let fm = FileManager.default
        if !fm.fileExists(atPath: dir.path) {
            do {
                try fm.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            } catch {
                throw SupervisorError.directory("Could not create \(dir.path): \(error.localizedDescription)")
            }
        }
        // attributesOfItem does not follow a final symbolic link, so a link planted in the
        // folder's place is seen as a link, not as the folder it points to.
        guard let attrs = try? fm.attributesOfItem(atPath: dir.path) else {
            throw SupervisorError.directory("Could not inspect \(dir.path).")
        }
        guard (attrs[.type] as? FileAttributeType) == .typeDirectory else {
            throw SupervisorError.directory("\(dir.path) is not a folder.")
        }
        guard let owner = (attrs[.ownerAccountID] as? NSNumber)?.uint32Value, owner == uid else {
            throw SupervisorError.directory("\(dir.path) belongs to another user; remove it and start Conductor again.")
        }
        let mode = (attrs[.posixPermissions] as? NSNumber)?.intValue ?? 0o777
        if mode & 0o077 != 0 {
            try? fm.setAttributes([.posixPermissions: 0o700], ofItemAtPath: dir.path)
        }
    }

    // MARK: - the cluster

    public func initializeCluster() async throws {
        guard let pg = config.postgres else { throw SupervisorError.noBundledPostgres }
        try? fm.createDirectory(at: pg.dataDirectory.deletingLastPathComponent(), withIntermediateDirectories: true)
        let result = try await runner.run(CommandSpec(pg.executable("initdb"), pg.initdbArguments,
                                                      environment: config.commandEnvironment))
        guard result.succeeded else { throw SupervisorError.initdb(result.failureMessage("initdb")) }
    }

    /// `conductor db restore --data-dir <dir> --backup latest`: the newest base backup and
    /// every WAL segment after it, from the bucket.
    public func restoreCluster(backup: String = "latest") async throws {
        guard let pg = config.postgres else { throw SupervisorError.noBundledPostgres }
        try? fm.createDirectory(at: pg.dataDirectory.deletingLastPathComponent(), withIntermediateDirectories: true)
        let result = try await conductor(ConductorCommands.dbRestore(dataDir: pg.dataDirectory, backup: backup))
        guard result.succeeded else { throw SupervisorError.restore(result.failureMessage("conductor db restore")) }
    }

    /// Writes the configuration the app owns and decides archiving. Returns whether anything
    /// Postgres reads at start changed, so a running server needs a restart.
    ///
    /// Archiving on: `conductor db archiving --data-dir <dir> --write`, before Postgres
    /// starts, as docs/STORAGE.md has it. Archiving off: the settings that command wrote are
    /// taken back out of `postgresql.auto.conf`, or a Postgres left with `archive_mode = on`
    /// and nowhere to archive would keep every WAL segment forever.
    @discardableResult
    public func configureCluster(storage: StorageShow?) async throws -> Bool {
        guard let pg = config.postgres else { return false }
        let confURL = pg.dataDirectory.appendingPathComponent(PostgresSetup.configFileName)
        let mainURL = pg.dataDirectory.appendingPathComponent("postgresql.conf")
        let autoURL = pg.dataDirectory.appendingPathComponent("postgresql.auto.conf")
        let before = [confURL, mainURL, autoURL].map { try? Data(contentsOf: $0) }

        do {
            try Data(pg.configuration.utf8).write(to: confURL, options: .atomic)
            let main = (try? String(contentsOf: mainURL, encoding: .utf8)) ?? ""
            if let updated = PostgresSetup.addingInclude(to: main) {
                try Data(updated.utf8).write(to: mainURL, options: .atomic)
            }
        } catch {
            throw SupervisorError.directory("Could not write the database configuration: \(error.localizedDescription)")
        }

        if storage?.databaseToBucket == true {
            let result = try await conductor(ConductorCommands.dbArchiving(dataDir: pg.dataDirectory))
            guard result.succeeded else { throw SupervisorError.archiving(result.failureMessage("conductor db archiving")) }
        } else if let auto = try? String(contentsOf: autoURL, encoding: .utf8),
                  let stripped = AutoConf.removingArchiveSettings(auto) {
            try? Data(stripped.utf8).write(to: autoURL, options: .atomic)
        }

        let after = [confURL, mainURL, autoURL].map { try? Data(contentsOf: $0) }
        return before != after
    }

    // MARK: - Postgres

    public func startPostgres(restart: Bool = false) async throws {
        guard let pg = config.postgres else { throw SupervisorError.noBundledPostgres }
        let agent = LaunchAgents.postgres(pg, paths: config.paths, environment: config.agentEnvironment,
                                          runAtLoad: config.startAtLogin)
        let plistChanged = try writePlist(agent)
        let loaded = try await launchctl.isLoaded(label: agent.label)
        if loaded && (plistChanged || restart) {
            // conductord holds connections, which a smart shutdown would wait on; stop it
            // first, and stop Postgres fast, so the restart is a clean one.
            try? await launchctl.bootout(label: LaunchAgents.daemonLabel)
            await stopPostgres()
            try await launchctl.replace(label: agent.label, plist: config.paths.launchAgentPlist(agent.label))
        } else if !loaded {
            try await launchctl.replace(label: agent.label, plist: config.paths.launchAgentPlist(agent.label))
        }
        if !(try await postgresReady()) {
            // Loaded but not running: an agent with RunAtLoad off, after a login.
            try? await launchctl.kickstart(label: agent.label)
        }
        try await waitForPostgres()
        let created = try await runner.run(CommandSpec(pg.executable("createdb"), pg.createDatabaseArguments,
                                                       environment: config.commandEnvironment))
        guard PostgresSetup.isAlreadyExists(created) else {
            throw SupervisorError.createDatabase(created.failureMessage("createdb"))
        }
    }

    /// A fast shutdown (clients disconnected, no crash recovery next time), then the job is
    /// unloaded. `pg_ctl stop` exits 0, so launchd, which restarts only on failure, leaves
    /// it stopped in between.
    public func stopPostgres() async {
        guard let pg = config.postgres else { return }
        _ = try? await runner.run(CommandSpec(pg.executable("pg_ctl"),
                                              ["stop", "-D", pg.dataDirectory.path, "-m", "fast", "-w", "-t", "30"],
                                              environment: config.commandEnvironment))
        try? await launchctl.bootout(label: LaunchAgents.postgresLabel)
    }

    public func postgresReady() async throws -> Bool {
        guard let pg = config.postgres else { return false }
        return try await runner.run(CommandSpec(pg.executable("pg_isready"), pg.isReadyArguments,
                                                environment: config.commandEnvironment)).succeeded
    }

    private func waitForPostgres() async throws {
        let step = 0.25
        var waited = 0.0
        while waited < probes.postgresTimeout {
            if try await postgresReady() { return }
            await probes.sleep(step)
            waited += step
        }
        throw SupervisorError.timeout("The database did not start within \(Int(probes.postgresTimeout)) seconds. Its log is \(config.paths.logFile("postgres.log").path).")
    }

    // MARK: - conductord

    public func startDaemon() async throws {
        guard let dsn = config.dsn else { throw SupervisorError.noBundledPostgres }
        let external = config.externalDSN != nil
        let agent = LaunchAgents.daemon(conductord: config.binaries.conductord, port: config.daemonPort, dsn: dsn,
                                        dsnInEnvironment: external, publicURL: config.publicURL,
                                        paths: config.paths, environment: config.agentEnvironment,
                                        runAtLoad: config.startAtLogin)
        let plistChanged = try writePlist(agent, privateFile: external)
        let loaded = try await launchctl.isLoaded(label: agent.label)
        if !loaded {
            // Not ours: something else answers, or holds the port, and conductord would fail
            // to bind it, over and over.
            if await probes.daemonHealthy(config.endpoint) || !probes.portFree(config.daemonPort) {
                throw SupervisorError.portTaken(config.daemonPort)
            }
        }
        if !loaded || plistChanged {
            try await launchctl.replace(label: agent.label, plist: config.paths.launchAgentPlist(agent.label))
        }
        if !(await probes.daemonHealthy(config.endpoint)) {
            try? await launchctl.kickstart(label: agent.label)
        }
        let step = 0.25
        var waited = 0.0
        while waited < probes.daemonTimeout {
            if await probes.daemonHealthy(config.endpoint) { return }
            await probes.sleep(step)
            waited += step
        }
        throw SupervisorError.timeout("The control plane did not answer at \(config.endpoint) within \(Int(probes.daemonTimeout)) seconds. Its log is \(config.paths.logFile("conductord.log").path).")
    }

    // MARK: - base backups

    /// With the database going to the bucket, the `dev.conductor.db-backup` agent runs
    /// `conductor db base-backup` every `base_backup_every_hours`. When the bucket holds no
    /// base backup yet, it is run once now: WAL alone restores nothing.
    public func configureBackups(storage: StorageShow?) async throws {
        let label = LaunchAgents.backupLabel
        guard let storage, storage.databaseToBucket, let dsn = config.dsn, config.postgres != nil else {
            try? await launchctl.bootout(label: label)
            try? fm.removeItem(at: config.paths.launchAgentPlist(label))
            return
        }
        let agent = LaunchAgents.baseBackup(conductor: config.binaries.conductor, dsn: dsn,
                                            hours: storage.settings.database.baseBackupEveryHours,
                                            paths: config.paths, environment: config.agentEnvironment)
        let changed = try writePlist(agent)
        let loaded = try await launchctl.isLoaded(label: label)
        if changed || !loaded {
            try await launchctl.replace(label: label, plist: config.paths.launchAgentPlist(label))
        }
        let listed = try await conductor(ConductorCommands.dbBackups)
        if listed.succeeded, let backups = BaseBackupList.decode(listed.stdout), backups.isEmpty {
            try? await launchctl.kickstart(label: label)
        }
    }

    // MARK: - commands

    /// Runs the bundled `conductor` with the app's environment.
    public func conductor(_ args: [String], stdin: Data? = nil, cwd: URL? = nil) async throws -> CommandResult {
        try await runner.run(CommandSpec(config.binaries.conductor, args, environment: config.commandEnvironment,
                                         currentDirectory: cwd, stdin: stdin))
    }

    /// `conductord bootstrap` for a repository: the organization, its project, the person as
    /// its first principal and this machine's owner, and the CLI's saved login. The DSN goes
    /// in DATABASE_URL, never on the command line.
    public func bootstrapOwner(repository: URL) async throws -> CommandResult {
        guard let dsn = config.dsn else { throw SupervisorError.noBundledPostgres }
        var env = config.commandEnvironment
        env["DATABASE_URL"] = dsn
        let result = try await runner.run(CommandSpec(config.binaries.conductord,
                                                      ConductorCommands.bootstrap(repository: repository, endpoint: config.endpoint),
                                                      environment: env, currentDirectory: repository))
        guard result.succeeded else { throw SupervisorError.bootstrap(result.failureMessage("conductord bootstrap")) }
        return result
    }

    // MARK: - plists

    /// Writes an agent's plist when its bytes differ from what is there; returns whether it
    /// did. A plist holding a password (an attached database's DSN) is readable by this user
    /// only.
    @discardableResult
    public func writePlist(_ agent: LaunchAgent, privateFile: Bool = false) throws -> Bool {
        let url = config.paths.launchAgentPlist(agent.label)
        let data: Data
        do { data = try agent.xml() } catch {
            throw SupervisorError.directory("Could not encode the \(agent.label) launch agent: \(error)")
        }
        if let existing = try? Data(contentsOf: url), existing == data { return false }
        do {
            try data.write(to: url, options: .atomic)
            try fm.setAttributes([.posixPermissions: privateFile ? 0o600 : 0o644], ofItemAtPath: url.path)
        } catch {
            throw SupervisorError.directory("Could not write \(url.path): \(error.localizedDescription)")
        }
        return true
    }
}

/// `postgresql.auto.conf` edits.
public enum AutoConf {
    /// The settings `conductor db archiving --write` adds (docs/STORAGE.md).
    public static let archiveSettings: Set<String> = ["archive_mode", "archive_command", "restore_command", "archive_timeout"]

    /// The file without the archiving settings, or nil when it has none. Postgres is stopped
    /// when this runs, which is when the file may be edited by hand.
    public static func removingArchiveSettings(_ text: String) -> String? {
        var removed = false
        let kept = text.split(separator: "\n", omittingEmptySubsequences: false).filter { line in
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            guard !trimmed.hasPrefix("#") else { return true }
            let name = trimmed.split(whereSeparator: { $0 == "=" || $0 == " " || $0 == "\t" }).first.map(String.init) ?? ""
            if archiveSettings.contains(name.lowercased()) {
                removed = true
                return false
            }
            return true
        }
        return removed ? kept.joined(separator: "\n") : nil
    }
}
