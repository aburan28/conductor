import Foundation

/// One launchd user agent, as data. `propertyList` is exactly what is written to
/// `~/Library/LaunchAgents/<label>.plist`, so a test reads back what a Mac would load.
///
/// Why plists driven by `launchctl` and not `SMAppService`: the agents run binaries at paths
/// that depend on where the app was installed and on the person's settings (port, data
/// folder, schedule), and `SMAppService` registers plists sealed into the bundle at build
/// time. A plain file under one label is also inspectable and removable without the app:
/// `launchctl print gui/$UID/dev.conductor.daemon`.
public struct LaunchAgent: Equatable, Sendable {
    public enum KeepAlive: Equatable, Sendable {
        /// Never restarted.
        case never
        /// Restarted whenever it exits.
        case always
        /// Restarted only when it exits non-zero or on a signal; a clean stop stays stopped.
        case onFailure
    }

    public var label: String
    public var programArguments: [String]
    public var environment: [String: String]
    public var workingDirectory: String?
    public var runAtLoad: Bool
    public var keepAlive: KeepAlive
    /// Seconds between runs, for a periodic job.
    public var startInterval: Int?
    public var standardOutPath: String?
    public var standardErrorPath: String?
    /// Seconds launchd waits after SIGTERM before SIGKILL.
    public var exitTimeOut: Int?
    /// "Background" for jobs a person is not waiting on, "Interactive" otherwise.
    public var processType: String?
    /// launchd waits this long before restarting a job that keeps exiting (default 10).
    public var throttleInterval: Int?

    public init(label: String, programArguments: [String], environment: [String: String] = [:],
                workingDirectory: String? = nil, runAtLoad: Bool = true, keepAlive: KeepAlive = .never,
                startInterval: Int? = nil, standardOutPath: String? = nil, standardErrorPath: String? = nil,
                exitTimeOut: Int? = nil, processType: String? = nil, throttleInterval: Int? = nil) {
        self.label = label
        self.programArguments = programArguments
        self.environment = environment
        self.workingDirectory = workingDirectory
        self.runAtLoad = runAtLoad
        self.keepAlive = keepAlive
        self.startInterval = startInterval
        self.standardOutPath = standardOutPath
        self.standardErrorPath = standardErrorPath
        self.exitTimeOut = exitTimeOut
        self.processType = processType
        self.throttleInterval = throttleInterval
    }

    /// The property list as a dictionary.
    public var propertyList: [String: Any] {
        var plist: [String: Any] = [
            "Label": label,
            "ProgramArguments": programArguments,
            "RunAtLoad": runAtLoad,
        ]
        if !environment.isEmpty { plist["EnvironmentVariables"] = environment }
        if let workingDirectory { plist["WorkingDirectory"] = workingDirectory }
        switch keepAlive {
        case .never: break
        case .always: plist["KeepAlive"] = true
        case .onFailure: plist["KeepAlive"] = ["SuccessfulExit": false]
        }
        if let startInterval { plist["StartInterval"] = startInterval }
        if let standardOutPath { plist["StandardOutPath"] = standardOutPath }
        if let standardErrorPath { plist["StandardErrorPath"] = standardErrorPath }
        if let exitTimeOut { plist["ExitTimeOut"] = exitTimeOut }
        if let processType { plist["ProcessType"] = processType }
        if let throttleInterval { plist["ThrottleInterval"] = throttleInterval }
        return plist
    }

    /// The property list as XML, the form launchd reads and a person can open.
    public func xml() throws -> Data {
        try PropertyListSerialization.data(fromPropertyList: propertyList, format: .xml, options: 0)
    }
}

/// The three agents the app owns, built from what the supervisor knows.
public enum LaunchAgents {
    public static let postgresLabel = "dev.conductor.postgres"
    public static let daemonLabel = "dev.conductor.daemon"
    public static let backupLabel = "dev.conductor.db-backup"
    public static let allLabels = [backupLabel, daemonLabel, postgresLabel]

    /// The private Postgres, restarted if it crashes. Its environment carries a PATH that
    /// finds the bundled `conductor`, because `archive_command` (`conductor db archive-wal`)
    /// is run by Postgres, with Postgres's environment.
    public static func postgres(_ setup: PostgresSetup, paths: AppPaths, environment: [String: String],
                                runAtLoad: Bool = true) -> LaunchAgent {
        LaunchAgent(
            label: postgresLabel,
            programArguments: setup.postgresArguments,
            environment: environment,
            workingDirectory: setup.dataDirectory.path,
            runAtLoad: runAtLoad,
            keepAlive: .onFailure,
            standardOutPath: paths.logFile("postgres.log").path,
            standardErrorPath: paths.logFile("postgres.log").path,
            // SIGTERM is Postgres's "smart" shutdown, which waits for conductord to let go of
            // its connections; conductord's own drain takes up to 25 s.
            exitTimeOut: 60,
            processType: "Background"
        )
    }

    /// conductord on loopback, against the private Postgres. The socket DSN carries no
    /// password (trust over a 0700 socket folder), so it sits on the command line, where
    /// `launchctl print` shows it. An attached database's DSN may hold one, so with
    /// `dsnInEnvironment` it goes in DATABASE_URL instead, as `conductor up` passes it, and
    /// the supervisor writes that plist readable by this user only.
    public static func daemon(conductord: URL, port: Int, dsn: String, dsnInEnvironment: Bool = false,
                              publicURL: String?, paths: AppPaths, environment: [String: String],
                              runAtLoad: Bool = true) -> LaunchAgent {
        var args = [conductord.path, "--addr", "127.0.0.1:\(port)"]
        var env = environment
        if dsnInEnvironment {
            env["DATABASE_URL"] = dsn
        } else {
            args += ["--dsn", dsn]
        }
        if let publicURL, !publicURL.isEmpty { args += ["--public-url", publicURL] }
        return LaunchAgent(
            label: daemonLabel,
            programArguments: args,
            environment: env,
            workingDirectory: paths.home.path,
            runAtLoad: runAtLoad,
            keepAlive: .always,
            standardOutPath: paths.logFile("conductord.log").path,
            standardErrorPath: paths.logFile("conductord.log").path,
            exitTimeOut: 30,
            processType: "Interactive"
        )
    }

    /// `conductor db base-backup` every `hours` hours (docs/STORAGE.md). Not run at load: the
    /// supervisor takes the first base backup itself once archiving is on.
    public static func baseBackup(conductor: URL, dsn: String, hours: Int, paths: AppPaths,
                                  environment: [String: String]) -> LaunchAgent {
        LaunchAgent(
            label: backupLabel,
            programArguments: [conductor.path] + ConductorCommands.dbBaseBackup(dsn: dsn),
            environment: environment,
            workingDirectory: paths.home.path,
            runAtLoad: false,
            keepAlive: .never,
            startInterval: max(1, hours) * 3600,
            standardOutPath: paths.logFile("db-backup.log").path,
            standardErrorPath: paths.logFile("db-backup.log").path,
            processType: "Background"
        )
    }
}
