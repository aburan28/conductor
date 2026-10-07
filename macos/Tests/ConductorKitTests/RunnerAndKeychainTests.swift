import XCTest
@testable import ConductorKit

final class RunnerAndKeychainTests: XCTestCase {
    let sh = URL(fileURLWithPath: "/bin/sh")

    func testStdinReachesTheCommandAndNeverItsArguments() async throws {
        let r = try await ProcessRunner().run(CommandSpec(sh, ["-c", "read line; printf 'got:%s' \"$line\""], stdin: Data("s3cr3t\n".utf8)))
        XCTAssertEqual(r.status, 0)
        XCTAssertEqual(r.stdoutText, "got:s3cr3t")
    }

    func testLargeOutputOnBothPipesDoesNotDeadlock() async throws {
        // 1 MiB on stdout and on stderr: far more than a pipe buffer holds.
        let script = "head -c 1048576 /dev/zero | tr '\\0' a; head -c 1048576 /dev/zero | tr '\\0' b >&2"
        let r = try await ProcessRunner().run(CommandSpec(sh, ["-c", script]))
        XCTAssertEqual(r.stdout.count, 1 << 20)
        XCTAssertEqual(r.stderr.count, 1 << 20)
    }

    func testACommandThatIgnoresItsInput() async throws {
        let big = Data(repeating: 0x41, count: 1 << 20)
        let r = try await ProcessRunner().run(CommandSpec(sh, ["-c", "exit 3"], stdin: big))
        XCTAssertEqual(r.status, 3)
    }

    func testStatusEnvironmentAndDirectory() async throws {
        let r = try await ProcessRunner().run(CommandSpec(sh, ["-c", "echo \"$GREETING\" in $(pwd); echo oops >&2; exit 7"],
                                                          environment: ["GREETING": "hello", "PATH": "/usr/bin:/bin"],
                                                          currentDirectory: URL(fileURLWithPath: "/")))
        XCTAssertEqual(r.status, 7)
        XCTAssertEqual(r.stdoutText, "hello in /\n")
        XCTAssertEqual(r.failureMessage("sh"), "oops")
        XCTAssertEqual(CommandResult(status: 4).failureMessage("conductor storage set"), "conductor storage set exited with status 4")
    }

    func testLaunchFailure() async {
        do {
            _ = try await ProcessRunner().run(CommandSpec(URL(fileURLWithPath: "/nonexistent/conductor"), []))
            XCTFail("expected a launch error")
        } catch let e as CommandError {
            XCTAssertTrue(e.description.contains("/nonexistent/conductor"))
        } catch {
            XCTFail("unexpected \(error)")
        }
    }

    func testSharedSecretsTrustTheSecurityTool() throws {
        let store = InMemorySecretStore()
        XCTAssertFalse(try store.exists(service: KeychainNames.s3Service, account: "AKIA1"))
        try store.writeShared("secret", service: KeychainNames.s3Service, account: "AKIA1", label: "Conductor S3")
        XCTAssertEqual(try store.read(service: KeychainNames.s3Service, account: "AKIA1"), "secret")
        XCTAssertEqual(store.item(service: KeychainNames.s3Service, account: "AKIA1")?.trustedPaths, ["/usr/bin/security"])
        try store.delete(service: KeychainNames.s3Service, account: "AKIA1")
        XCTAssertNil(try store.read(service: KeychainNames.s3Service, account: "AKIA1"))
        XCTAssertEqual(KeychainNames.sealService, "dev.conductor.seal")
        XCTAssertEqual(KeychainNames.sealAccount, "default")
        XCTAssertEqual(KeychainNames.tokenService, "dev.conductor")
    }

    #if canImport(Security) && os(macOS)
    /// Writes and reads the real login keychain, then checks that /usr/bin/security reads the
    /// item without a prompt, which is the point of the access list. Off by default: a CI
    /// runner's keychain may be locked, and a prompt would hang the job.
    func testKeychainRoundTripWithTheSecurityTool() async throws {
        try XCTSkipUnless(ProcessInfo.processInfo.environment["CONDUCTOR_TEST_KEYCHAIN"] == "1",
                          "set CONDUCTOR_TEST_KEYCHAIN=1 to touch the login keychain")
        let store = KeychainStore()
        let service = "dev.conductor.test.\(UUID().uuidString)"
        defer { try? store.delete(service: service, account: "acct") }
        try store.writeShared("value-1", service: service, account: "acct", label: "Conductor test")
        XCTAssertTrue(try store.exists(service: service, account: "acct"))
        XCTAssertEqual(try store.read(service: service, account: "acct"), "value-1")
        let r = try await ProcessRunner().run(CommandSpec(URL(fileURLWithPath: "/usr/bin/security"),
                                                          ["find-generic-password", "-s", service, "-a", "acct", "-w"]))
        XCTAssertEqual(r.stdoutText.trimmingCharacters(in: .newlines), "value-1")
    }
    #endif
}
