import Foundation
#if canImport(Darwin)
import Darwin
#elseif canImport(Glibc)
import Glibc
#elseif canImport(Musl)
import Musl
#endif

/// Every place on disk the app reads or writes, from one home directory, so a test can lay
/// the whole tree out under a temporary folder.
public struct AppPaths: Equatable, Sendable {
    public var home: URL
    /// `~/Library/Application Support/Conductor`.
    public var support: URL
    /// The private Postgres cluster: `~/Library/Application Support/Conductor/pg`.
    public var postgresData: URL
    /// Where Postgres puts its Unix socket. Inside the support folder when the socket's path
    /// fits in a `sockaddr_un`, else a per-user folder under /tmp (see `socketDirectory`).
    public var socketDirectory: URL
    /// `~/Library/Logs/Conductor`: each launchd agent's stdout and stderr.
    public var logs: URL
    /// `~/Library/LaunchAgents`.
    public var launchAgents: URL
    /// The CLI's state folder: `$CONDUCTOR_STATE_DIR`, else `~/.conductor`. The app and the
    /// CLI share it, so `storage.json` and the saved login are the same files for both.
    public var conductorState: URL

    /// The longest socket path a Unix-domain socket address holds on macOS: `sun_path` is 104
    /// bytes, one of them the terminating NUL.
    public static let maxSocketPathBytes = 103

    public init(home: URL, environment: [String: String] = [:], uid: UInt32, postgresPort: Int = PostgresSetup.defaultPort) {
        self.home = home
        support = home.appendingPathComponent("Library/Application Support/Conductor", isDirectory: true)
        postgresData = support.appendingPathComponent("pg", isDirectory: true)
        logs = home.appendingPathComponent("Library/Logs/Conductor", isDirectory: true)
        launchAgents = home.appendingPathComponent("Library/LaunchAgents", isDirectory: true)
        // Read exactly as the CLI reads it (internal/storage.Path): no tilde expansion.
        if let state = environment["CONDUCTOR_STATE_DIR"], !state.isEmpty {
            conductorState = URL(fileURLWithPath: state, isDirectory: true)
        } else {
            conductorState = home.appendingPathComponent(".conductor", isDirectory: true)
        }
        socketDirectory = Self.socketDirectory(support: support, uid: uid, port: postgresPort)
    }

    /// This user's paths.
    public static func current() -> AppPaths {
        AppPaths(home: FileManager.default.homeDirectoryForCurrentUser,
                 environment: ProcessInfo.processInfo.environment,
                 uid: UInt32(getuid()))
    }

    /// `storage.json`, beside the CLI's other state (docs/STORAGE.md).
    public var storageFile: URL { conductorState.appendingPathComponent("storage.json") }

    public func logFile(_ name: String) -> URL { logs.appendingPathComponent(name) }

    public func launchAgentPlist(_ label: String) -> URL {
        launchAgents.appendingPathComponent("\(label).plist")
    }

    /// The socket folder: `<support>/run` when `<support>/run/.s.PGSQL.<port>` fits in a
    /// socket address, which it does for every ordinary home directory. A long user name or
    /// a relocated home can push it past 103 bytes, where Postgres refuses to start; then a
    /// folder of this user's own under /tmp, which the supervisor creates 0700 and checks it
    /// owns before Postgres uses it.
    public static func socketDirectory(support: URL, uid: UInt32, port: Int) -> URL {
        let preferred = support.appendingPathComponent("run", isDirectory: true)
        if socketPath(in: preferred, port: port).utf8.count <= maxSocketPathBytes {
            return preferred
        }
        return URL(fileURLWithPath: "/tmp/dev.conductor.\(uid)", isDirectory: true)
    }

    /// The socket file Postgres creates in `directory` for `port`.
    public static func socketPath(in directory: URL, port: Int) -> String {
        directory.appendingPathComponent(".s.PGSQL.\(port)").path
    }
}
