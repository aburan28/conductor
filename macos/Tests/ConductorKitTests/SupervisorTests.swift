import XCTest
@testable import ConductorKit
#if canImport(Glibc)
import Glibc
#endif

/// A Mac in miniature: launchd agents load and unload, initdb makes a cluster, Postgres is
/// ready once its agent is loaded, conductord healthy once its agent is. Every command is
/// recorded, so a test reads back exactly what the supervisor would run.
final class FakeMac: @unchecked Sendable {
    private let lock = NSLock()
    private(set) var loaded = Set<String>()
    var backupsJSON = "[]"
    var archivingLine = "archive_mode = 'on'\narchive_command = 'conductor db archive-wal %p %f'\n"
    let dataDir: URL
    lazy var runner = RecordingRunner { [unowned self] spec in self.respond(spec) }

    init(dataDir: URL) { self.dataDir = dataDir }

    func isLoaded(_ label: String) -> Bool {
        lock.lock(); defer { lock.unlock() }
        return loaded.contains(label)
    }

    func setLoaded(_ label: String, _ on: Bool) {
        lock.lock(); defer { lock.unlock() }
        if on { loaded.insert(label) } else { loaded.remove(label) }
    }

    func respond(_ spec: CommandSpec) -> CommandResult {
        let name = spec.executable.lastPathComponent
        let args = spec.arguments
        switch name {
        case "launchctl":
            let label = args.last.map { ($0 as NSString).lastPathComponent.replacingOccurrences(of: ".plist", with: "") } ?? ""
            switch args.first {
            case "print": return CommandResult(status: isLoaded(String(label.split(separator: "/").last ?? "")) ? 0 : 113)
            case "bootstrap": setLoaded(label, true); return CommandResult(status: 0)
            case "bootout": setLoaded(String(label.split(separator: "/").last ?? ""), false); return CommandResult(status: 0)
            default: return CommandResult(status: 0)
            }
        case "initdb":
            try? FileManager.default.createDirectory(at: dataDir, withIntermediateDirectories: true)
            try? "17\n".write(to: dataDir.appendingPathComponent("PG_VERSION"), atomically: true, encoding: .utf8)
            try? "# initdb\n".write(to: dataDir.appendingPathComponent("postgresql.conf"), atomically: true, encoding: .utf8)
            try? "# Do not edit\n".write(to: dataDir.appendingPathComponent("postgresql.auto.conf"), atomically: true, encoding: .utf8)
            return CommandResult(status: 0)
        case "pg_isready":
            return CommandResult(status: isLoaded(LaunchAgents.postgresLabel) ? 0 : 2)
        case "createdb":
            return CommandResult(status: 1, stderr: Data("database \"conductor\" already exists".utf8))
        case "conductor":
            if args.starts(with: ["db", "archiving"]) {
                // Appends, as docs/STORAGE.md says, but only once.
                let auto = dataDir.appendingPathComponent("postgresql.auto.conf")
                let text = (try? String(contentsOf: auto, encoding: .utf8)) ?? ""
                if !text.contains("archive_mode") {
                    try? (text + archivingLine).write(to: auto, atomically: true, encoding: .utf8)
                }
            }
            if args.starts(with: ["db", "restore"]) {
                try? FileManager.default.createDirectory(at: dataDir, withIntermediateDirectories: true)
                try? "17\n".write(to: dataDir.appendingPathComponent("PG_VERSION"), atomically: true, encoding: .utf8)
            }
            if args.starts(with: ["db", "backups"]) {
                return CommandResult(status: 0, stdout: Data(backupsJSON.utf8))
            }
            return CommandResult(status: 0)
        default:
            return CommandResult(status: 0)
        }
    }
}

final class SupervisorTests: XCTestCase {
    var home: URL!
    var fake: FakeMac!

    override func setUp() {
        home = FileManager.default.temporaryDirectory.appendingPathComponent("sup-\(UUID().uuidString)")
        try? FileManager.default.createDirectory(at: home, withIntermediateDirectories: true)
        fake = FakeMac(dataDir: home.appendingPathComponent("Library/Application Support/Conductor/pg"))
    }

    override func tearDown() {
        try? FileManager.default.removeItem(at: home)
    }

    private var uid: UInt32 { UInt32(getuid()) }

    private func supervisor(storage: AppSettings = AppSettings(), externalDSN: String? = nil, portFree: Bool = true,
                            foreignDaemon: Bool = false) -> Supervisor {
        let paths = AppPaths(home: home, uid: uid)
        let res = URL(fileURLWithPath: "/Applications/Conductor.app/Contents/Resources")
        let binaries = ConductorBinaries(conductor: res.appendingPathComponent("bin/conductor"),
                                         conductord: res.appendingPathComponent("bin/conductord"),
                                         conductorMCP: res.appendingPathComponent("bin/conductor-mcp"),
                                         postgresBin: res.appendingPathComponent("postgres/bin"))
        let config = SupervisorConfig(paths: paths, binaries: binaries, settings: storage, uid: uid,
                                      baseEnvironment: ["PATH": "/usr/bin:/bin", "HOME": home.path], externalDSN: externalDSN)
        let fake = self.fake!
        let probes = SupervisorProbes(daemonHealthy: { _ in foreignDaemon || fake.isLoaded(LaunchAgents.daemonLabel) },
                                      portFree: { _ in portFree }, sleep: { _ in }, postgresTimeout: 1, daemonTimeout: 1)
        return Supervisor(config: config, runner: fake.runner, probes: probes)
    }

    private func storage(database: Bool, hours: Int = 24) throws -> StorageShow {
        try StorageShow.decode(Data("""
        {"configured":true,"path":"x","source":"file","s3":{"bucket":"b"},"auth":{"method":"environment"},
         "uses":{"sessions":true,"checkpoints":true,"database":\(database)},
         "database":{"archive_wal":true,"archive_timeout_seconds":60,"base_backup_every_hours":\(hours),"keep_base_backups":7,"seal":true}}
        """.utf8))
    }

    private var uidDomain: String { "gui/\(uid)" }

    func testFirstStartWithTheDatabaseGoingToTheBucket() async throws {
        let sup = supervisor()
        try await sup.ensureRunning(storage: try storage(database: true, hours: 12))
        let t = fake.runner.transcript
        let plist = { (label: String) in self.home.appendingPathComponent("Library/LaunchAgents/\(label).plist").path }

        XCTAssertEqual(t.first, "initdb --pgdata \(fake.dataDir.path) --username conductor --encoding UTF8 --locale C --auth-local trust --auth-host reject --no-instructions")
        // Archiving is written before Postgres first starts.
        let archiving = try XCTUnwrap(t.firstIndex(of: "conductor db archiving --data-dir \(fake.dataDir.path) --write"))
        let postgres = try XCTUnwrap(t.firstIndex(of: "launchctl bootstrap \(uidDomain) \(plist("dev.conductor.postgres"))"))
        let daemon = try XCTUnwrap(t.firstIndex(of: "launchctl bootstrap \(uidDomain) \(plist("dev.conductor.daemon"))"))
        let backup = try XCTUnwrap(t.firstIndex(of: "launchctl bootstrap \(uidDomain) \(plist("dev.conductor.db-backup"))"))
        XCTAssertLessThan(archiving, postgres)
        XCTAssertLessThan(postgres, daemon)
        XCTAssertLessThan(daemon, backup)
        XCTAssertTrue(t.contains { $0.hasPrefix("createdb -h ") })
        // No base backup in the bucket yet: one is taken now.
        XCTAssertEqual(t.last, "launchctl kickstart \(uidDomain)/dev.conductor.db-backup")
        let phase = await sup.phase
        XCTAssertEqual(phase, .running)

        // What the app owns in the data directory.
        let conf = try String(contentsOf: fake.dataDir.appendingPathComponent("conductor.conf"), encoding: .utf8)
        XCTAssertTrue(conf.contains("listen_addresses = ''"))
        let main = try String(contentsOf: fake.dataDir.appendingPathComponent("postgresql.conf"), encoding: .utf8)
        XCTAssertTrue(main.hasSuffix("include_if_exists = 'conductor.conf'\n"))

        // The plists on disk are the ones the builders make.
        let backupPlist = try PropertyListSerialization.propertyList(from: Data(contentsOf: URL(fileURLWithPath: plist("dev.conductor.db-backup"))), format: nil) as? [String: Any]
        XCTAssertEqual(backupPlist?["StartInterval"] as? Int, 12 * 3600)
        let daemonPlist = try PropertyListSerialization.propertyList(from: Data(contentsOf: URL(fileURLWithPath: plist("dev.conductor.daemon"))), format: nil) as? [String: Any]
        let args = try XCTUnwrap(daemonPlist?["ProgramArguments"] as? [String])
        XCTAssertEqual(Array(args.prefix(3)), ["/Applications/Conductor.app/Contents/Resources/bin/conductord", "--addr", "127.0.0.1:8080"])

        // The socket folder exists and is private.
        let attrs = try FileManager.default.attributesOfItem(atPath: AppPaths(home: home, uid: uid).socketDirectory.path)
        XCTAssertEqual((attrs[.posixPermissions] as? NSNumber)?.intValue, 0o700)
    }

    func testASecondStartChangesNothing() async throws {
        let sup = supervisor()
        let s = try storage(database: true)
        try await sup.ensureRunning(storage: s)
        let before = fake.runner.commands.count
        fake.backupsJSON = #"[{"id":"b1","taken_at":"2026-10-07T00:00:00Z"}]"#
        try await sup.ensureRunning(storage: s)
        let second = Array(fake.runner.transcript.dropFirst(before))
        XCTAssertFalse(second.contains { $0.hasPrefix("initdb") })
        XCTAssertFalse(second.contains { $0.contains("bootstrap") || $0.contains("bootout") || $0.contains("kickstart") },
                       "nothing changed, so nothing restarts: \(second)")
        XCTAssertTrue(second.contains("conductor db archiving --data-dir \(fake.dataDir.path) --write"))
    }

    func testDatabaseOffLeavesArchivingOffAndRemovesTheBackupAgent() async throws {
        let sup = supervisor()
        try await sup.ensureRunning(storage: try storage(database: true))
        let auto = fake.dataDir.appendingPathComponent("postgresql.auto.conf")
        XCTAssertTrue(try String(contentsOf: auto, encoding: .utf8).contains("archive_mode"))
        let before = fake.runner.commands.count

        try await sup.ensureRunning(storage: try storage(database: false))
        let second = Array(fake.runner.transcript.dropFirst(before))
        XCTAssertFalse(second.contains { $0.contains("db archiving") })
        XCTAssertFalse(try String(contentsOf: auto, encoding: .utf8).contains("archive_mode"), "archiving settings are taken back out")
        // The configuration changed, so Postgres restarts cleanly: daemon off first, fast stop.
        let bootoutDaemon = try XCTUnwrap(second.firstIndex(of: "launchctl bootout \(uidDomain)/dev.conductor.daemon"))
        let fastStop = try XCTUnwrap(second.firstIndex { $0.hasPrefix("pg_ctl stop -D ") && $0.contains("-m fast") })
        XCTAssertLessThan(bootoutDaemon, fastStop)
        XCTAssertTrue(second.contains("launchctl bootout \(uidDomain)/dev.conductor.db-backup"))
        XCTAssertFalse(FileManager.default.fileExists(atPath: home.appendingPathComponent("Library/LaunchAgents/dev.conductor.db-backup.plist").path))
    }

    func testNoBucketMeansNoArchivingAndNoBackupAgent() async throws {
        try await supervisor().ensureRunning(storage: nil)
        let t = fake.runner.transcript
        XCTAssertFalse(t.contains { $0.contains("db archiving") || $0.contains("db backups") })
        XCTAssertFalse(t.contains { $0.contains("bootstrap") && $0.contains("db-backup") })
    }

    func testRestoreFromTheBucketInsteadOfInitdb() async throws {
        try await supervisor().ensureRunning(storage: try storage(database: true), restoreFromBucket: true)
        let t = fake.runner.transcript
        XCTAssertFalse(t.contains { $0.hasPrefix("initdb") })
        let restore = try XCTUnwrap(t.firstIndex(of: "conductor db restore --data-dir \(fake.dataDir.path) --backup latest"))
        let archiving = try XCTUnwrap(t.firstIndex { $0.hasPrefix("conductor db archiving") })
        XCTAssertLessThan(restore, archiving)
    }

    func testAPortSomeoneElseHoldsIsRefused() async throws {
        do {
            try await supervisor(foreignDaemon: true).ensureRunning(storage: nil)
            XCTFail("expected portTaken")
        } catch let e as SupervisorError {
            XCTAssertEqual(e, .portTaken(8080))
        }
        do {
            try await supervisor(portFree: false).ensureRunning(storage: nil)
            XCTFail("expected portTaken")
        } catch let e as SupervisorError {
            XCTAssertEqual(e, .portTaken(8080))
        }
    }

    func testAttachedDatabaseRunsNoPostgresAndHidesItsPassword() async throws {
        let dsn = "postgres://conductor:pw@db.lan/conductor"
        try await supervisor(externalDSN: dsn).ensureRunning(storage: try storage(database: true))
        let t = fake.runner.transcript
        XCTAssertFalse(t.contains { $0.hasPrefix("initdb") || $0.contains("postgres") && $0.contains("bootstrap") })
        XCTAssertFalse(t.contains { $0.contains("bootstrap") && $0.contains("db-backup") }, "base backups are for the bundled cluster")
        let url = home.appendingPathComponent("Library/LaunchAgents/dev.conductor.daemon.plist")
        let plist = try PropertyListSerialization.propertyList(from: Data(contentsOf: url), format: nil) as? [String: Any]
        XCTAssertFalse((plist?["ProgramArguments"] as? [String] ?? []).contains(dsn))
        XCTAssertEqual((plist?["EnvironmentVariables"] as? [String: String])?["DATABASE_URL"], dsn)
        let mode = (try FileManager.default.attributesOfItem(atPath: url.path)[.posixPermissions] as? NSNumber)?.intValue
        XCTAssertEqual(mode, 0o600)
    }

    func testBootstrapKeepsTheDSNOffTheCommandLine() async throws {
        let sup = supervisor()
        _ = try await sup.bootstrapOwner(repository: URL(fileURLWithPath: "/Users/ada/src/app"))
        let spec = try XCTUnwrap(fake.runner.commands.last)
        XCTAssertEqual(spec.executable.lastPathComponent, "conductord")
        XCTAssertEqual(spec.arguments, ["bootstrap", "--repo", "/Users/ada/src/app", "--endpoint", "http://127.0.0.1:8080"])
        XCTAssertTrue(spec.environment?["DATABASE_URL"]?.contains("host='") ?? false)
        XCTAssertEqual(spec.currentDirectory?.path, "/Users/ada/src/app")
    }

    func testConductorRunsWithTheBundledPath() async throws {
        let sup = supervisor()
        _ = try await sup.conductor(["storage", "set", "--json"], stdin: Data("s\n".utf8))
        let spec = try XCTUnwrap(fake.runner.commands.last)
        XCTAssertEqual(spec.stdin, Data("s\n".utf8))
        XCTAssertTrue(spec.environment?["PATH"]?.hasPrefix("/Applications/Conductor.app/Contents/Resources/bin:") ?? false)
    }

    func testAForeignSocketFolderIsRefused() throws {
        let dir = home.appendingPathComponent("sock")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o755])
        XCTAssertThrowsError(try Supervisor.preparePrivateDirectory(dir, uid: uid &+ 1))
        try Supervisor.preparePrivateDirectory(dir, uid: uid)
        let mode = (try FileManager.default.attributesOfItem(atPath: dir.path)[.posixPermissions] as? NSNumber)?.intValue
        XCTAssertEqual(mode, 0o700, "tightened")
        let link = home.appendingPathComponent("link")
        try FileManager.default.createSymbolicLink(at: link, withDestinationURL: dir)
        XCTAssertThrowsError(try Supervisor.preparePrivateDirectory(link, uid: uid), "a symbolic link is not the folder")
    }
}
