import XCTest
@testable import ConductorKit

final class LaunchAgentTests: XCTestCase {
    let home = URL(fileURLWithPath: "/Users/ada", isDirectory: true)
    var paths: AppPaths { AppPaths(home: home, uid: 501) }
    var setup: PostgresSetup {
        PostgresSetup(paths: paths, binDirectory: URL(fileURLWithPath: "/Applications/Conductor.app/Contents/Resources/postgres/bin"))
    }

    /// Writes the plist as XML and reads it back the way launchd would.
    private func roundTrip(_ agent: LaunchAgent) throws -> [String: Any] {
        let data = try agent.xml()
        XCTAssertTrue(String(decoding: data, as: UTF8.self).hasPrefix("<?xml"))
        let plist = try PropertyListSerialization.propertyList(from: data, options: [], format: nil)
        return try XCTUnwrap(plist as? [String: Any])
    }

    func testPostgresAgent() throws {
        let agent = LaunchAgents.postgres(setup, paths: paths, environment: ["PATH": "/x", "HOME": home.path])
        let plist = try roundTrip(agent)
        XCTAssertEqual(plist["Label"] as? String, "dev.conductor.postgres")
        XCTAssertEqual(plist["ProgramArguments"] as? [String], [
            "/Applications/Conductor.app/Contents/Resources/postgres/bin/postgres",
            "-D", "/Users/ada/Library/Application Support/Conductor/pg",
        ])
        XCTAssertEqual(plist["RunAtLoad"] as? Bool, true)
        XCTAssertEqual((plist["KeepAlive"] as? [String: Any])?["SuccessfulExit"] as? Bool, false)
        XCTAssertEqual(plist["StandardErrorPath"] as? String, "/Users/ada/Library/Logs/Conductor/postgres.log")
        XCTAssertEqual(plist["WorkingDirectory"] as? String, "/Users/ada/Library/Application Support/Conductor/pg")
        XCTAssertEqual((plist["EnvironmentVariables"] as? [String: String])?["PATH"], "/x")
        XCTAssertEqual(plist["ExitTimeOut"] as? Int, 60)
    }

    func testDaemonAgent() throws {
        let agent = LaunchAgents.daemon(conductord: URL(fileURLWithPath: "/A/bin/conductord"), port: 8080, dsn: setup.dsn,
                                        publicURL: "https://mac.tail.ts.net", paths: paths, environment: [:])
        let plist = try roundTrip(agent)
        XCTAssertEqual(plist["Label"] as? String, "dev.conductor.daemon")
        XCTAssertEqual(plist["ProgramArguments"] as? [String], [
            "/A/bin/conductord", "--addr", "127.0.0.1:8080", "--dsn", setup.dsn,
            "--public-url", "https://mac.tail.ts.net",
        ])
        XCTAssertEqual(plist["KeepAlive"] as? Bool, true)
        XCTAssertNil(plist["EnvironmentVariables"])
    }

    func testDaemonAgentKeepsAPasswordOffTheCommandLine() throws {
        let dsn = "postgres://u:secret@db.lan/conductor"
        let agent = LaunchAgents.daemon(conductord: URL(fileURLWithPath: "/A/bin/conductord"), port: 8081, dsn: dsn,
                                        dsnInEnvironment: true, publicURL: nil, paths: paths, environment: ["HOME": "/Users/ada"])
        XCTAssertFalse(agent.programArguments.contains(dsn))
        XCTAssertFalse(agent.programArguments.contains("--dsn"))
        XCTAssertEqual(agent.environment["DATABASE_URL"], dsn)
        XCTAssertEqual(agent.environment["HOME"], "/Users/ada")
    }

    func testBaseBackupAgentRunsOnASchedule() throws {
        let agent = LaunchAgents.baseBackup(conductor: URL(fileURLWithPath: "/A/bin/conductor"), dsn: setup.dsn, hours: 6,
                                            paths: paths, environment: [:])
        let plist = try roundTrip(agent)
        XCTAssertEqual(plist["Label"] as? String, "dev.conductor.db-backup")
        XCTAssertEqual(plist["ProgramArguments"] as? [String], ["/A/bin/conductor", "db", "base-backup", "--dsn", setup.dsn])
        XCTAssertEqual(plist["StartInterval"] as? Int, 6 * 3600)
        XCTAssertEqual(plist["RunAtLoad"] as? Bool, false)
        XCTAssertNil(plist["KeepAlive"])
        XCTAssertEqual(LaunchAgents.baseBackup(conductor: URL(fileURLWithPath: "/c"), dsn: "", hours: 0, paths: paths,
                                               environment: [:]).startInterval, 3600, "at least hourly")
    }

    func testStartAtLoginOff() throws {
        let agent = LaunchAgents.postgres(setup, paths: paths, environment: [:], runAtLoad: false)
        XCTAssertEqual(try roundTrip(agent)["RunAtLoad"] as? Bool, false)
    }

    func testLaunchAgentPlistPath() {
        XCTAssertEqual(paths.launchAgentPlist("dev.conductor.daemon").path, "/Users/ada/Library/LaunchAgents/dev.conductor.daemon.plist")
    }

    func testLaunchctlCommands() async throws {
        let runner = RecordingRunner { spec in CommandResult(status: spec.arguments.first == "print" ? 113 : 0) }
        let l = Launchctl(uid: 501, runner: runner)
        try await l.replace(label: "dev.conductor.daemon", plist: URL(fileURLWithPath: "/p/dev.conductor.daemon.plist"))
        let loaded = try await l.isLoaded(label: "dev.conductor.daemon")
        XCTAssertFalse(loaded)
        try await l.kickstart(label: "dev.conductor.db-backup")
        XCTAssertEqual(runner.transcript, [
            "launchctl bootout gui/501/dev.conductor.daemon",
            "launchctl bootstrap gui/501 /p/dev.conductor.daemon.plist",
            "launchctl print gui/501/dev.conductor.daemon",
            "launchctl kickstart gui/501/dev.conductor.db-backup",
        ])
    }

    func testLaunchctlBootstrapFailureIsAnError() async {
        let runner = RecordingRunner { spec in
            spec.arguments.first == "bootstrap" ? CommandResult(status: 5, stderr: Data("Bootstrap failed: 5: Input/output error".utf8))
                                                : CommandResult(status: 0)
        }
        do {
            try await Launchctl(uid: 501, runner: runner).replace(label: "x", plist: URL(fileURLWithPath: "/x.plist"))
            XCTFail("expected an error")
        } catch let e as SupervisorError {
            XCTAssertEqual(e, .launchctl("bootstrap x", "Bootstrap failed: 5: Input/output error"))
        } catch {
            XCTFail("unexpected \(error)")
        }
    }
}
