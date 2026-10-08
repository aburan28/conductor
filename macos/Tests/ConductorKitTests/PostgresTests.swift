import XCTest
@testable import ConductorKit

final class PostgresTests: XCTestCase {
    let home = URL(fileURLWithPath: "/Users/ada", isDirectory: true)

    func testPathsLiveInApplicationSupport() {
        let p = AppPaths(home: home, uid: 501)
        XCTAssertEqual(p.support.path, "/Users/ada/Library/Application Support/Conductor")
        XCTAssertEqual(p.postgresData.path, "/Users/ada/Library/Application Support/Conductor/pg")
        XCTAssertEqual(p.socketDirectory.path, "/Users/ada/Library/Application Support/Conductor/run")
        XCTAssertEqual(p.logs.path, "/Users/ada/Library/Logs/Conductor")
        XCTAssertEqual(p.storageFile.path, "/Users/ada/.conductor/storage.json")
    }

    func testStateDirectoryFromTheEnvironment() {
        let p = AppPaths(home: home, environment: ["CONDUCTOR_STATE_DIR": "/srv/conductor-state"], uid: 501)
        XCTAssertEqual(p.storageFile.path, "/srv/conductor-state/storage.json")
    }

    func testSocketFallsBackWhenThePathIsTooLong() {
        let longHome = URL(fileURLWithPath: "/Users/" + String(repeating: "a", count: 60), isDirectory: true)
        let p = AppPaths(home: longHome, uid: 501)
        XCTAssertEqual(p.socketDirectory.path, "/tmp/dev.conductor.501")
        let normal = AppPaths(home: home, uid: 501)
        XCTAssertLessThanOrEqual(AppPaths.socketPath(in: normal.socketDirectory, port: 5432).utf8.count, AppPaths.maxSocketPathBytes)
    }

    func testInitdbArguments() {
        let s = PostgresSetup(paths: AppPaths(home: home, uid: 501), binDirectory: URL(fileURLWithPath: "/b"))
        XCTAssertEqual(s.initdbArguments, [
            "--pgdata", "/Users/ada/Library/Application Support/Conductor/pg",
            "--username", "conductor", "--encoding", "UTF8", "--locale", "C",
            "--auth-local", "trust", "--auth-host", "reject", "--no-instructions",
        ])
        XCTAssertEqual(s.executable("initdb").path, "/b/initdb")
        XCTAssertEqual(s.postgresArguments, ["/b/postgres", "-D", "/Users/ada/Library/Application Support/Conductor/pg"])
    }

    func testConfigurationListensOnTheSocketOnly() {
        let s = PostgresSetup(paths: AppPaths(home: home, uid: 501), binDirectory: URL(fileURLWithPath: "/b"))
        let conf = s.configuration
        XCTAssertTrue(conf.contains("listen_addresses = ''\n"))
        XCTAssertTrue(conf.contains("unix_socket_directories = '\"/Users/ada/Library/Application Support/Conductor/run\"'\n"))
        XCTAssertTrue(conf.contains("unix_socket_permissions = 0700\n"))
        XCTAssertTrue(conf.contains("jit = off\n"))
        XCTAssertTrue(conf.contains("wal_level = replica\n"))
        XCTAssertTrue(conf.contains("port = 5432\n"))
    }

    func testQuoting() {
        XCTAssertEqual(PostgresSetup.confQuote("it's a \\ path"), "'it''s a \\\\ path'")
        XCTAssertEqual(PostgresSetup.socketDirectoryList(["/a b", "/c\"d"]), "\"/a b\",\"/c\"\"d\"")
        XCTAssertEqual(PostgresSetup.conninfoQuote("/a b/it's"), "'/a b/it\\'s'")
    }

    func testDSN() {
        let s = PostgresSetup(paths: AppPaths(home: home, uid: 501), binDirectory: URL(fileURLWithPath: "/b"))
        XCTAssertEqual(s.dsn, "host='/Users/ada/Library/Application Support/Conductor/run' port=5432 user='conductor' dbname='conductor' sslmode=disable")
        XCTAssertFalse(s.dsn.contains("password"))
        XCTAssertEqual(s.maintenanceDSN, "host='/Users/ada/Library/Application Support/Conductor/run' port=5432 user='conductor' dbname='postgres' sslmode=disable")
        XCTAssertEqual(s.inRecoveryArguments, ["-h", "/Users/ada/Library/Application Support/Conductor/run", "-p", "5432",
                                               "-U", "conductor", "-d", "postgres", "-X", "-A", "-t", "-c", "select pg_is_in_recovery()"])
    }

    func testIncludeIsAddedOnce() throws {
        let original = "# initdb's file\nshared_buffers = 64MB"
        let once = try XCTUnwrap(PostgresSetup.addingInclude(to: original))
        XCTAssertTrue(once.hasPrefix(original + "\n"))
        XCTAssertTrue(once.hasSuffix("include_if_exists = 'conductor.conf'\n"))
        XCTAssertNil(PostgresSetup.addingInclude(to: once))
    }

    func testPgIsReadyAndCreatedb() {
        let s = PostgresSetup(binDirectory: URL(fileURLWithPath: "/b"), dataDirectory: URL(fileURLWithPath: "/d"),
                              socketDirectory: URL(fileURLWithPath: "/s"), port: 5433)
        XCTAssertEqual(s.isReadyArguments, ["-h", "/s", "-p", "5433", "-U", "conductor", "-d", "postgres", "-q"])
        XCTAssertEqual(s.createDatabaseArguments, ["-h", "/s", "-p", "5433", "-U", "conductor", "conductor"])
        XCTAssertTrue(PostgresSetup.isAlreadyExists(CommandResult(status: 1, stderr: Data("createdb: error: database creation failed: ERROR:  database \"conductor\" already exists".utf8))))
        XCTAssertFalse(PostgresSetup.isAlreadyExists(CommandResult(status: 1, stderr: Data("could not connect".utf8))))
    }

    func testAutoConfArchiveSettingsAreRemoved() throws {
        let auto = """
        # Do not edit this file manually!
        # It will be overwritten by the ALTER SYSTEM command.
        work_mem = '8MB'
        archive_mode = 'on'
        archive_command = 'conductor db archive-wal %p %f'
        archive_timeout = '60'
        """
        let stripped = try XCTUnwrap(AutoConf.removingArchiveSettings(auto))
        XCTAssertEqual(stripped, "# Do not edit this file manually!\n# It will be overwritten by the ALTER SYSTEM command.\nwork_mem = '8MB'")
        XCTAssertNil(AutoConf.removingArchiveSettings(stripped))
    }
}
