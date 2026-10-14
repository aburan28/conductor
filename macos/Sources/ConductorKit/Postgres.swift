import Foundation

/// The private Postgres the app runs: where its binaries and data are, how it is initialised,
/// the configuration the app owns, and how everything else reaches it.
///
/// It listens on a Unix socket only (`listen_addresses = ''`), in a folder only this user can
/// open, so there is no TCP port to collide with a Postgres the person already runs, and
/// nothing on the network or under another account can connect. Local connections are
/// trusted for that reason: the socket folder's permissions are the authentication, and the
/// DSN carries no password that could leak through `ps` or a launchd plist.
public struct PostgresSetup: Equatable, Sendable {
    public static let defaultPort = 5432
    /// The name of the file the app owns inside the data directory, rewritten on every start.
    public static let configFileName = "conductor.conf"
    public static let includeLine = "include_if_exists = '\(configFileName)'"

    /// `Contents/Resources/postgres/bin` in the app bundle.
    public var binDirectory: URL
    public var dataDirectory: URL
    public var socketDirectory: URL
    public var port: Int
    public var user: String
    public var database: String

    public init(binDirectory: URL, dataDirectory: URL, socketDirectory: URL,
                port: Int = PostgresSetup.defaultPort, user: String = "conductor", database: String = "conductor") {
        self.binDirectory = binDirectory
        self.dataDirectory = dataDirectory
        self.socketDirectory = socketDirectory
        self.port = port
        self.user = user
        self.database = database
    }

    public init(paths: AppPaths, binDirectory: URL, port: Int = PostgresSetup.defaultPort) {
        self.init(binDirectory: binDirectory, dataDirectory: paths.postgresData,
                  socketDirectory: paths.socketDirectory, port: port)
    }

    public func executable(_ name: String) -> URL { binDirectory.appendingPathComponent(name) }

    /// Whether a cluster has been initialised (or restored) in the data directory.
    public func isInitialized(fileManager: FileManager = .default) -> Bool {
        fileManager.fileExists(atPath: dataDirectory.appendingPathComponent("PG_VERSION").path)
    }

    // MARK: - commands

    /// `initdb`: UTF-8 with the C locale, so sorting never changes under an OS update; the
    /// superuser named `user`; trust on the socket and nothing over TCP.
    public var initdbArguments: [String] {
        [
            "--pgdata", dataDirectory.path,
            "--username", user,
            "--encoding", "UTF8",
            "--locale", "C",
            "--auth-local", "trust",
            "--auth-host", "reject",
            "--no-instructions",
        ]
    }

    /// The server itself, in the foreground, as launchd runs it.
    public var postgresArguments: [String] {
        [executable("postgres").path, "-D", dataDirectory.path]
    }

    /// `pg_isready`: exit 0 once the server accepts connections on the socket.
    public var isReadyArguments: [String] {
        ["-h", socketDirectory.path, "-p", String(port), "-U", user, "-d", "postgres", "-q"]
    }

    /// `createdb` for the application database. Run after every start; "already exists" is
    /// success (see `isAlreadyExists`).
    public var createDatabaseArguments: [String] {
        ["-h", socketDirectory.path, "-p", String(port), "-U", user, database]
    }

    public static func isAlreadyExists(_ result: CommandResult) -> Bool {
        result.succeeded || result.stderrText.contains("already exists")
    }

    /// What conductord, `conductor db base-backup` and `conductord bootstrap` connect with: a
    /// libpq keyword/value string, which pgx reads too. The socket folder can contain spaces
    /// ("Application Support"), so every value is quoted.
    public var dsn: String { dsn(database: database) }

    /// The same socket, the `postgres` maintenance database: what `conductor db
    /// base-backup` connects with (pg_basebackup takes a replication connection, which no
    /// application database is needed for).
    public var maintenanceDSN: String { dsn(database: "postgres") }

    func dsn(database: String) -> String {
        [
            "host=\(Self.conninfoQuote(socketDirectory.path))",
            "port=\(port)",
            "user=\(Self.conninfoQuote(user))",
            "dbname=\(Self.conninfoQuote(database))",
            "sslmode=disable",
        ].joined(separator: " ")
    }

    /// `psql … -c "select pg_is_in_recovery()"`: prints `t` while a restored cluster is still
    /// replaying archived WAL (read-only), `f` once it has promoted itself.
    public var inRecoveryArguments: [String] {
        ["-h", socketDirectory.path, "-p", String(port), "-U", user, "-d", "postgres",
         "-X", "-A", "-t", "-c", "select pg_is_in_recovery()"]
    }

    /// `recovery.signal` in the data directory: `conductor db restore` wrote it, and Postgres
    /// has not finished the recovery it asks for.
    public func isRecovering(fileManager: FileManager = .default) -> Bool {
        fileManager.fileExists(atPath: dataDirectory.appendingPathComponent("recovery.signal").path)
    }

    // MARK: - configuration

    /// The settings the app owns. Written to `conductor.conf`, which `postgresql.conf`
    /// includes at its end; `postgresql.auto.conf` (ALTER SYSTEM, and `conductor db
    /// archiving --write`) is read after both, so archiving settings written there win.
    public var configuration: String {
        """
        # Written by Conductor.app every time it starts Postgres; edits here are overwritten.
        # Put your own settings in postgresql.conf, after its include line.
        listen_addresses = ''
        port = \(port)
        unix_socket_directories = \(Self.confQuote(Self.socketDirectoryList([socketDirectory.path])))
        unix_socket_permissions = 0700
        max_connections = 50
        shared_buffers = 128MB
        # No JIT: the hardened runtime the signed app runs under forbids writable code pages.
        jit = off
        # WAL fit for archiving and base backups (docs/STORAGE.md), whether or not a bucket is set.
        wal_level = replica
        max_wal_senders = 5
        # stderr goes to the launchd agent's log file.
        logging_collector = off
        log_destination = 'stderr'
        log_line_prefix = '%m [%p] '

        """
    }

    /// `postgresql.conf` with the include line appended once. Returns nil when it is already
    /// there, so the file is not rewritten for nothing.
    public static func addingInclude(to postgresqlConf: String) -> String? {
        let present = postgresqlConf
            .split(whereSeparator: \.isNewline)
            .contains { $0.trimmingCharacters(in: .whitespaces) == includeLine }
        if present { return nil }
        var text = postgresqlConf
        if !text.isEmpty && !text.hasSuffix("\n") { text += "\n" }
        text += "\n# Conductor.app's settings (\(configFileName)); anything below this line overrides them.\n"
        text += includeLine + "\n"
        return text
    }

    /// A postgresql.conf string literal: single quotes, with quotes doubled and backslashes
    /// escaped, since the configuration parser reads C-style escapes inside quotes.
    public static func confQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "'", with: "''") + "'"
    }

    /// The value of `unix_socket_directories`: a comma-separated list in which each folder is
    /// double-quoted (a folder name may contain spaces or commas) with quotes doubled.
    public static func socketDirectoryList(_ directories: [String]) -> String {
        directories.map { "\"" + $0.replacingOccurrences(of: "\"", with: "\"\"") + "\"" }.joined(separator: ",")
    }

    /// A libpq conninfo value: single-quoted, with backslashes and single quotes escaped by a
    /// backslash.
    public static func conninfoQuote(_ value: String) -> String {
        "'" + value.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "'", with: "\\'") + "'"
    }
}
