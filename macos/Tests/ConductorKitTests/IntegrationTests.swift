import XCTest
@testable import ConductorKit
#if canImport(FoundationNetworking)
import FoundationNetworking
#endif
#if canImport(Glibc)
import Glibc
#endif

/// The supervisor's Postgres and conductord for real, without launchd: initdb with the kit's
/// arguments, the kit's configuration (in a home folder whose path has a space, like
/// "Application Support"), Postgres on its socket only, conductord on the kit's DSN, then
/// bootstrap, local sign-in, the status API, the event stream, and an invite link that
/// `conductor join` accepts.
///
/// Off unless pointed at binaries, since it needs Postgres and a Go build:
///
///   CONDUCTOR_IT_PG_BIN=/usr/lib/postgresql/17/bin   initdb, postgres, pg_isready, createdb, pg_ctl
///   CONDUCTOR_IT_BIN=/path/to/bin                    conductord and conductor (`make build`)
///   CONDUCTOR_IT_RUN_AS=postgres                     when the tests run as root, which Postgres refuses
final class IntegrationTests: XCTestCase {
    var env: [String: String] { ProcessInfo.processInfo.environment }

    /// argv run as the configured user when there is one.
    func spec(_ exe: URL, _ args: [String], environment: [String: String]? = nil, stdin: Data? = nil) -> CommandSpec {
        if let user = env["CONDUCTOR_IT_RUN_AS"], !user.isEmpty {
            var full = ["-u", user, "--"]
            if let environment {
                full += ["/usr/bin/env"] + environment.map { "\($0.key)=\($0.value)" }
            }
            full += [exe.path] + args
            return CommandSpec(URL(fileURLWithPath: "/usr/sbin/runuser"), full, stdin: stdin)
        }
        return CommandSpec(exe, args, environment: environment, stdin: stdin)
    }

    func testPostgresAndConductordEndToEnd() async throws {
        guard let pgBin = env["CONDUCTOR_IT_PG_BIN"], !pgBin.isEmpty else {
            throw XCTSkip("set CONDUCTOR_IT_PG_BIN (and CONDUCTOR_IT_BIN) to run the integration test")
        }
        let fm = FileManager.default
        let root = URL(fileURLWithPath: "/tmp/conductor-it-\(UUID().uuidString.prefix(8))")
        let home = root.appendingPathComponent("Ada Home", isDirectory: true)
        try fm.createDirectory(at: home, withIntermediateDirectories: true)
        defer { try? fm.removeItem(at: root) }
        let runner = ProcessRunner()

        var runAsUID = UInt32(getuid())
        if let user = env["CONDUCTOR_IT_RUN_AS"], !user.isEmpty, let pw = getpwnam(user) {
            runAsUID = pw.pointee.pw_uid
        }
        let paths = AppPaths(home: home, uid: runAsUID, postgresPort: 5432)
        let setup = PostgresSetup(paths: paths, binDirectory: URL(fileURLWithPath: pgBin))
        try fm.createDirectory(at: paths.support, withIntermediateDirectories: true)
        try Supervisor.preparePrivateDirectory(paths.socketDirectory, uid: UInt32(getuid()))
        if runAsUID != getuid() {
            _ = try await runner.run(CommandSpec(URL(fileURLWithPath: "/bin/chown"), ["-R", "\(runAsUID)", root.path]))
        }

        // initdb, exactly the kit's arguments.
        let initdb = try await runner.run(spec(setup.executable("initdb"), setup.initdbArguments))
        XCTAssertEqual(initdb.status, 0, initdb.stderrText)

        // The configuration the supervisor writes.
        let conf = setup.dataDirectory.appendingPathComponent(PostgresSetup.configFileName)
        let main = setup.dataDirectory.appendingPathComponent("postgresql.conf")
        try setup.configuration.write(to: conf, atomically: true, encoding: .utf8)
        let mainText = try String(contentsOf: main, encoding: .utf8)
        try XCTUnwrap(PostgresSetup.addingInclude(to: mainText)).write(to: main, atomically: true, encoding: .utf8)
        if runAsUID != getuid() {
            _ = try await runner.run(CommandSpec(URL(fileURLWithPath: "/bin/chown"), ["-R", "\(runAsUID)", root.path]))
        }

        // Postgres in the foreground, as launchd runs it.
        let pgSpec = spec(URL(fileURLWithPath: setup.postgresArguments[0]), Array(setup.postgresArguments.dropFirst()))
        let pg = Process()
        pg.executableURL = pgSpec.executable
        pg.arguments = pgSpec.arguments
        let pgLog = root.appendingPathComponent("postgres.log")
        _ = fm.createFile(atPath: pgLog.path, contents: nil)
        pg.standardError = try FileHandle(forWritingTo: pgLog)
        pg.standardOutput = FileHandle.nullDevice
        try pg.run()
        defer {
            _ = try? ProcessRunner.runBlocking(spec(setup.executable("pg_ctl"), ["stop", "-D", setup.dataDirectory.path, "-m", "fast", "-w"]))
            pg.waitUntilExit()
        }

        var ready = false
        for _ in 0..<120 {
            if try await runner.run(CommandSpec(setup.executable("pg_isready"), setup.isReadyArguments)).succeeded { ready = true; break }
            try await Task.sleep(nanoseconds: 250_000_000)
        }
        XCTAssertTrue(ready, (try? String(contentsOf: pgLog, encoding: .utf8)) ?? "")
        let sockets = try fm.contentsOfDirectory(atPath: paths.socketDirectory.path)
        XCTAssertTrue(sockets.contains(".s.PGSQL.5432"), "listens on the socket in the private folder: \(sockets)")
        let created = try await runner.run(CommandSpec(setup.executable("createdb"), setup.createDatabaseArguments))
        XCTAssertTrue(PostgresSetup.isAlreadyExists(created), created.stderrText)
        let again = try await runner.run(CommandSpec(setup.executable("createdb"), setup.createDatabaseArguments))
        XCTAssertTrue(PostgresSetup.isAlreadyExists(again), "a second start tolerates the database: \(again.stderrText)")

        guard let bin = env["CONDUCTOR_IT_BIN"], !bin.isEmpty else { return }
        let conductord = URL(fileURLWithPath: bin).appendingPathComponent("conductord")
        let conductor = URL(fileURLWithPath: bin).appendingPathComponent("conductor")
        let port = try XCTUnwrap(PortProbe.firstFree(from: 18080, count: 50))
        let endpoint = "http://127.0.0.1:\(port)"
        let agent = LaunchAgents.daemon(conductord: conductord, port: port, dsn: setup.dsn, publicURL: nil, paths: paths, environment: [:])
        let cliEnv = ["HOME": home.path, "PATH": "/usr/bin:/bin", "CONDUCTOR_STATE_DIR": paths.conductorState.path]

        // conductord with the launch agent's own arguments: the kit's DSN, through pgx.
        let daemon = Process()
        daemon.executableURL = URL(fileURLWithPath: agent.programArguments[0])
        daemon.arguments = Array(agent.programArguments.dropFirst())
        daemon.environment = cliEnv
        let dLog = root.appendingPathComponent("conductord.log")
        _ = fm.createFile(atPath: dLog.path, contents: nil)
        daemon.standardError = try FileHandle(forWritingTo: dLog)
        daemon.standardOutput = try FileHandle(forWritingTo: dLog)
        try daemon.run()
        defer { daemon.terminate(); daemon.waitUntilExit() }

        let api = APIClient(endpoint: URL(string: endpoint)!)
        var healthy = false
        for _ in 0..<240 {
            if await api.isHealthy() { healthy = true; break }
            try await Task.sleep(nanoseconds: 250_000_000)
        }
        XCTAssertTrue(healthy, (try? String(contentsOf: dLog, encoding: .utf8)) ?? "")
        guard healthy else { return }

        // A fresh database: nobody owns the machine, so onboarding goes to the repository.
        let before = try await api.localStatus()
        XCTAssertEqual(OnboardingFlow.signIn(before), .needsOwner, "\(before)")

        // The repository step: conductor init, then conductord bootstrap with the DSN in the
        // environment, exactly as the supervisor runs it.
        let repo = home.appendingPathComponent("src/my-app", isDirectory: true)
        try fm.createDirectory(at: repo, withIntermediateDirectories: true)
        let initRepo = try await runner.run(CommandSpec(conductor, ConductorCommands.initRepository(repo), environment: cliEnv))
        XCTAssertEqual(initRepo.status, 0, initRepo.stderrText)
        var bootEnv = cliEnv
        bootEnv["DATABASE_URL"] = setup.dsn
        bootEnv["USER"] = "ada"
        let boot = try await runner.run(CommandSpec(conductord, ConductorCommands.bootstrap(repository: repo, endpoint: endpoint),
                                                    environment: bootEnv, currentDirectory: repo))
        XCTAssertEqual(boot.status, 0, boot.stderrText)

        // Local sign-in, as the app does it.
        let after = try await api.localStatus()
        XCTAssertEqual(OnboardingFlow.signIn(after), .automatic, "\(after)")
        let session = try await api.localSession()
        XCTAssertFalse(session.token.isEmpty)
        let authed = api.withToken(session.token)
        let who = try await authed.whoami()
        XCTAssertEqual(who.principal.handle, "ada")
        let project = try XCTUnwrap(who.projects.first?.slug)
        XCTAssertEqual(project, "my-app")

        // Something on the event stream: a task, created through the CLI's own login.
        let task = try await runner.run(CommandSpec(conductor, ["task", "create", "--project", project, "--title", "Try the app", "--scope", "path:README.md"],
                                                    environment: cliEnv))
        XCTAssertEqual(task.status, 0, task.stderrText + task.stdoutText)

        // A login that hit its limit, reported the way `conductor wrap` reports it: the server
        // turns it into a quota.exhausted event on the project's stream.
        let now = ISO8601DateFormatter().string(from: Date())
        let quota = #"{"project":"\#(project)","snapshots":[{"harness":"claude","account":"default","window":"five_hour","window_minutes":300,"used_percent":100,"limit_reached":true,"source":"integration-test","source_kind":"manual","observed_at":"\#(now)"}]}"#
        let (qBody, qStatus) = try await authed.send(authed.request("POST", "/v1/quota", body: Data(quota.utf8)))
        XCTAssertEqual(qStatus, 200, String(decoding: qBody, as: UTF8.self))

        let status = try await authed.status(project: project)
        XCTAssertEqual(status.project.map { !$0.isEmpty }, true)

        let received = Received()
        let stream = EventStream(url: authed.eventStreamURL(project: project), token: session.token,
                                 onEvent: { received.add($0) }, onState: { received.state($0) })
        stream.start()
        for _ in 0..<40 where received.events.isEmpty { try await Task.sleep(nanoseconds: 250_000_000) }
        stream.stop()
        XCTAssertTrue(received.states.contains(.open), "\(received.states)")
        let first = try XCTUnwrap(received.events.first, "the backlog carries the quota event")
        let decoded = try XCTUnwrap(DomainEvent.decode(first))
        XCTAssertEqual(decoded.type, first.event)
        XCTAssertEqual(decoded.type, "quota.exhausted")
        guard case .usageLimit(let harness, _) = EventReaction.classify(decoded) else {
            return XCTFail("a usage limit: \(decoded)")
        }
        XCTAssertEqual(harness, "claude")

        // Invite someone, and the link joins from another login.
        let invite = try await authed.invite(project: project, InviteRequest(handle: "rachel", role: "contributor", tokenTTL: InviteExpiry.week.ttl))
        let token = try XCTUnwrap(invite.token)
        let link = InviteLink.web(endpoint: endpoint, project: project, token: token)
        let appLink = InviteLink.app(endpoint: endpoint, project: project, token: token)
        XCTAssertEqual(try InviteLink.parse(appLink).get().webLink, link)
        let otherHome = root.appendingPathComponent("Rachel", isDirectory: true)
        try fm.createDirectory(at: otherHome, withIntermediateDirectories: true)
        let joined = try await runner.run(CommandSpec(conductor, ConductorCommands.join(link: link),
                                                      environment: ["HOME": otherHome.path, "PATH": "/usr/bin:/bin"]))
        XCTAssertEqual(joined.status, 0, joined.stderrText)
        let result = try JSONDecoder().decode(JoinResult.self, from: joined.stdout)
        XCTAssertEqual(result.handle, "rachel")
        XCTAssertEqual(result.project, project)
    }

    final class Received: @unchecked Sendable {
        private let lock = NSLock()
        private var _events: [SSEEvent] = []
        private var _states: [EventStream.State] = []
        func add(_ e: SSEEvent) { lock.lock(); _events.append(e); lock.unlock() }
        func state(_ s: EventStream.State) { lock.lock(); _states.append(s); lock.unlock() }
        var events: [SSEEvent] { lock.lock(); defer { lock.unlock() }; return _events }
        var states: [EventStream.State] { lock.lock(); defer { lock.unlock() }; return _states }
    }
}
