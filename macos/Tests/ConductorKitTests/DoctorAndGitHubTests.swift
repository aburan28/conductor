import XCTest
@testable import ConductorKit

final class DoctorAndGitHubTests: XCTestCase {
    func testDoctorReport() throws {
        let json = """
        {
          "endpoint": "http://127.0.0.1:8080", "reachable": true, "principal": "ada", "project": "conductor",
          "client_version": "v0.9.0", "server_version": "v0.9.0", "database": "ok", "database_ok": true,
          "docker": "not installed", "harnesses": [{"kind": "claude", "available": true, "supports_mcp": true,
          "supports_reasoning_effort": true, "supports_structured_events": true, "supports_resume": true}],
          "integrations": [
            {"tool": "claude", "title": "Claude Code", "detected": true, "configured": true, "transport": "stdio",
             "config_path": "~/.claude.json", "hooks_supported": true, "hooks": true},
            {"tool": "codex", "title": "Codex", "detected": true, "configured": false, "hooks_supported": false, "hooks": false,
             "fix": "conductor integrate codex --global"},
            {"tool": "cursor", "title": "Cursor", "detected": true, "configured": true, "hooks_supported": true, "hooks": false},
            {"tool": "zed", "title": "Zed", "detected": false, "configured": false, "hooks_supported": false, "hooks": false}
          ]
        }
        """
        let r = try DoctorReport.decode(Data(json.utf8))
        XCTAssertTrue(r.reachable)
        XCTAssertTrue(r.databaseOK)
        XCTAssertEqual(r.integrations.map(\.state), [.connected, .notConnected, .connectedWithoutHooks, .notInstalled])
        XCTAssertEqual(r.toolsToConnect.map(\.tool), ["codex", "cursor"])
        XCTAssertTrue(r.anyToolConnected)
        XCTAssertEqual(r.integrations[1].fix, "conductor integrate codex --global")
    }

    func testDoctorWithNothingInstalled() throws {
        let r = try DoctorReport.decode(Data(#"{"endpoint":"","reachable":false,"client_version":"v1","database":"","database_ok":false,"docker":"","harnesses":null,"integrations":null}"#.utf8))
        XCTAssertFalse(r.anyToolConnected)
        XCTAssertEqual(r.integrations, [])
    }

    func testGitHubStatusStages() throws {
        let none = try GitHubStatus.decode(Data(#"{"configured":false,"mode":"","app":null,"install_url":"","installations":null,"installations_error":"","permission_gaps":null,"linked":null,"last_poll":null,"last_error":""}"#.utf8))
        XCTAssertEqual(none.stage, .noApp)
        let json = """
        {"configured": true, "mode": "polling",
         "app": {"id": 123, "slug": "conductor-ada-mac", "name": "Conductor ada-mac", "owner": "ada", "html_url": "https://github.com/apps/conductor-ada-mac"},
         "install_url": "https://github.com/apps/conductor-ada-mac/installations/new",
         "installations": [], "installations_error": "",
         "permission_gaps": [{"scope": "app", "missing": "issues:write", "fix": "accept it", "url": "https://github.com/settings/apps/x/permissions"}],
         "linked": [{"project": "conductor", "repository": "aburan28/conductor", "issues": "label:conductor"}],
         "last_poll": "2026-10-07T10:00:00Z", "last_error": ""}
        """
        var st = try GitHubStatus.decode(Data(json.utf8))
        XCTAssertEqual(st.stage, .notInstalled)
        XCTAssertEqual(st.app?.slug, "conductor-ada-mac")
        XCTAssertEqual(st.linked.first?["repository"], "aburan28/conductor")
        XCTAssertEqual(st.permissionGaps.first?.missing, "issues:write")
        st.installations = [.init(id: 9, account: "aburan28", targetType: "User", htmlURL: nil)]
        XCTAssertEqual(st.stage, .installed)
    }

    func testGitHubSetupAndCommands() throws {
        let setup = try JSONDecoder().decode(GitHubSetup.self, from: Data(#"{"setup_url":"http://127.0.0.1:8080/github/setup/abc","expires_at":"2026-10-07T11:00:00Z","name":"Conductor ada-mac 1a2b","webhooks":false}"#.utf8))
        XCTAssertEqual(setup.setupURL, "http://127.0.0.1:8080/github/setup/abc")
        XCTAssertEqual(setup.webhooks, false)
        XCTAssertEqual(ConductorCommands.githubSetup(), ["github", "setup", "--json", "--no-open"])
        XCTAssertEqual(ConductorCommands.githubSetup(org: "acme", replace: true), ["github", "setup", "--json", "--no-open", "--org", "acme", "--replace"])
        XCTAssertEqual(ConductorCommands.githubLink(), ["github", "link", "--json"])
        XCTAssertEqual(ConductorCommands.githubLink(repository: "acme/widgets", project: "widgets"), ["github", "link", "acme/widgets", "--project", "widgets", "--json"])
        XCTAssertEqual(ConductorCommands.githubStatus, ["github", "status", "--json"])
    }

    func testOtherCommandLines() {
        XCTAssertEqual(ConductorCommands.doctor, ["doctor", "--json"])
        XCTAssertEqual(ConductorCommands.integrateAll, ["integrate", "all", "--global"])
        XCTAssertEqual(ConductorCommands.initRepository(URL(fileURLWithPath: "/r")), ["init", "--dir", "/r"])
        XCTAssertEqual(ConductorCommands.bootstrap(repository: URL(fileURLWithPath: "/r"), endpoint: "http://127.0.0.1:8080"),
                       ["bootstrap", "--repo", "/r", "--endpoint", "http://127.0.0.1:8080"])
        XCTAssertEqual(ConductorCommands.join(link: "https://a/#token=t"), ["join", "https://a/#token=t", "--json", "--no-integrate"])
        XCTAssertEqual(ConductorCommands.checkpointList, ["checkpoint", "list", "--json"])
        XCTAssertEqual(ConductorCommands.dbArchiving(dataDir: URL(fileURLWithPath: "/d")), ["db", "archiving", "--data-dir", "/d", "--write"])
        XCTAssertEqual(ConductorCommands.dbRestore(dataDir: URL(fileURLWithPath: "/d")), ["db", "restore", "--data-dir", "/d", "--backup", "latest"])
        XCTAssertEqual(ConductorCommands.dbStatus, ["db", "status", "--json"])
        XCTAssertEqual(ConductorCommands.dbBackups, ["db", "backups", "--json"])
        XCTAssertEqual(ConductorCommands.storageTest, ["storage", "test", "--json"])
    }

    func testSecurityStatus() throws {
        let s = try JSONDecoder().decode(SecurityStatus.self, from: Data(#"{"security_mode":"local","mode_source":"flag","owner":"ada","you_are_owner":true,"behind_proxy":false}"#.utf8))
        XCTAssertEqual(s.securityMode, "local")
        XCTAssertTrue(s.isPinned)
    }

    func testBinariesAndEnvironment() {
        let resources = URL(fileURLWithPath: "/Applications/Conductor.app/Contents/Resources")
        let found = ConductorBinaries.locate(resources: resources, environment: [:], isExecutable: { $0.hasPrefix(resources.path) })
        XCTAssertEqual(found?.conductor.path, "/Applications/Conductor.app/Contents/Resources/bin/conductor")
        XCTAssertEqual(found?.postgresBin?.path, "/Applications/Conductor.app/Contents/Resources/postgres/bin")
        let noPG = ConductorBinaries.locate(resources: resources, environment: [:], isExecutable: { $0.contains("/bin/conductor") })
        XCTAssertNil(noPG?.postgresBin)
        let override = ConductorBinaries.locate(resources: resources, environment: ["CONDUCTOR_BIN_DIR": "/dev/bin"], isExecutable: { _ in true })
        XCTAssertEqual(override?.conductord.path, "/dev/bin/conductord")
        XCTAssertNil(ConductorBinaries.locate(resources: nil, environment: [:], isExecutable: { _ in false }))

        let env = found!.environment(base: ["PATH": "/usr/bin:/bin:/custom", "SECRET": "x"], home: URL(fileURLWithPath: "/Users/ada"))
        XCTAssertEqual(env["PATH"], "/Applications/Conductor.app/Contents/Resources/bin:/Applications/Conductor.app/Contents/Resources/postgres/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/custom:/usr/sbin:/sbin")
        XCTAssertEqual(env["HOME"], "/Users/ada")
        let agent = found!.agentEnvironment(base: ["PATH": "/usr/bin", "SECRET": "x", "CONDUCTOR_STATE_DIR": "/s", "AWS_PROFILE": "dev"], home: URL(fileURLWithPath: "/Users/ada"))
        XCTAssertNil(agent["SECRET"], "a launch agent gets only what it needs")
        XCTAssertEqual(agent["CONDUCTOR_STATE_DIR"], "/s")
        XCTAssertEqual(agent["AWS_PROFILE"], "dev")
        XCTAssertNotNil(agent["PATH"])
    }
}
