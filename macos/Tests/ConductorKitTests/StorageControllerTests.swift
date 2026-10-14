import XCTest
@testable import ConductorKit

/// A SecretStore that refuses writes, as the Keychain does when it cannot build an access list.
final class RefusingStore: SecretStore, @unchecked Sendable {
    func read(service: String, account: String) throws -> String? { nil }
    func exists(service: String, account: String) throws -> Bool { false }
    func write(_ secret: String, service: String, account: String, label: String, trustedPaths: [String]) throws {
        throw SecretStoreError.access("no access list")
    }
    func delete(service: String, account: String) throws {}
}

final class StorageControllerTests: XCTestCase {
    final class Calls: @unchecked Sendable {
        private let lock = NSLock()
        private var _calls: [([String], Data?)] = []
        var calls: [([String], Data?)] { lock.lock(); defer { lock.unlock() }; return _calls }
        func add(_ a: [String], _ d: Data?) { lock.lock(); _calls.append((a, d)); lock.unlock() }
    }

    private func controller(_ secrets: SecretStore, calls: Calls, respond: @escaping @Sendable ([String]) -> CommandResult = { _ in
        CommandResult(status: 0, stdout: Data(StorageTests.showJSON.utf8))
    }) -> StorageController {
        StorageController(secrets: secrets) { args, stdin in
            calls.add(args, stdin)
            return respond(args)
        }
    }

    private func staticForm(secret: String) -> StorageForm {
        var f = StorageForm()
        f.bucket = "b"
        f.accessKeyID = "AKIA1"
        f.newSecret = secret
        f.seal = false
        return f
    }

    func testSaveWritesTheSecretToTheKeychainAndTellsTheCLI() async throws {
        let store = InMemorySecretStore()
        let calls = Calls()
        let show = try await controller(store, calls: calls).save(staticForm(secret: "wJalr"))
        XCTAssertEqual(show.settings.s3.bucket, "my-team-conductor")
        let item = try XCTUnwrap(store.item(service: "dev.conductor.s3", account: "AKIA1"))
        XCTAssertEqual(item.secret, "wJalr")
        XCTAssertEqual(item.trustedPaths, ["/usr/bin/security"])
        XCTAssertEqual(calls.calls.count, 1)
        let (args, stdin) = calls.calls[0]
        XCTAssertTrue(args.contains("--secret-from=keychain"))
        XCTAssertNil(stdin, "nothing on standard input when the item is already there")
        XCTAssertFalse(args.joined().contains("wJalr"))
    }

    func testSaveFallsBackToStandardInput() async throws {
        let calls = Calls()
        _ = try await controller(RefusingStore(), calls: calls).save(staticForm(secret: "wJalr"))
        let (args, stdin) = calls.calls[0]
        XCTAssertTrue(args.contains("--secret-from=stdin"))
        XCTAssertTrue(args.contains("--secret-store=keychain"))
        XCTAssertEqual(stdin, Data("wJalr\n".utf8))
        XCTAssertFalse(args.joined().contains("wJalr"))
    }

    func testSaveWithTheSecretAlreadyStored() async throws {
        let store = InMemorySecretStore()
        try store.writeShared("old", service: "dev.conductor.s3", account: "AKIA1", label: "x")
        let calls = Calls()
        _ = try await controller(store, calls: calls).save(staticForm(secret: ""))
        XCTAssertTrue(calls.calls[0].0.contains("--secret-from=keychain"))
        XCTAssertEqual(try store.read(service: "dev.conductor.s3", account: "AKIA1"), "old")
    }

    func testSaveRefusesAnIncompleteForm() async {
        let calls = Calls()
        do {
            _ = try await controller(InMemorySecretStore(), calls: calls).save(staticForm(secret: ""))
            XCTFail("expected a refusal")
        } catch let e as StorageCommandError {
            XCTAssertEqual(e, .invalid(["Enter the secret access key."]))
        } catch {
            XCTFail("unexpected \(error)")
        }
        XCTAssertTrue(calls.calls.isEmpty, "nothing runs")
    }

    func testSealPassphrase() async throws {
        let store = InMemorySecretStore()
        let calls = Calls()
        let c = controller(store, calls: calls)
        var f = staticForm(secret: "s")
        f.seal = true
        XCTAssertEqual(c.problems(f), ["Set a seal passphrase, or turn sealing off."])
        XCTAssertEqual(c.problems(f, environment: ["CONDUCTOR_CHECKPOINT_KEY": "k"]), [])
        f.newSealPassphrase = "correct horse"
        _ = try await c.save(f)
        let item = try XCTUnwrap(store.item(service: "dev.conductor.seal", account: "default"))
        XCTAssertEqual(item.secret, "correct horse")
        XCTAssertEqual(item.trustedPaths, ["/usr/bin/security"])
        XCTAssertTrue(c.sealPassphraseSet)
        XCTAssertFalse(calls.calls[0].0.joined().contains("horse"))
    }

    func testSaveReportsTheCLIsRefusal() async {
        let calls = Calls()
        let c = controller(InMemorySecretStore(), calls: calls) { _ in
            CommandResult(status: 1, stderr: Data("conductor: --secret-from keychain: not found in the Keychain\n".utf8))
        }
        do {
            _ = try await c.save(staticForm(secret: "s"))
            XCTFail("expected an error")
        } catch {
            XCTAssertEqual(error as? StorageCommandError, .command("conductor: --secret-from keychain: not found in the Keychain"))
        }
    }

    func testTestDecodesAFailure() async {
        let json = #"{"ok":false,"credentials":"","location":"s3://b/conductor","steps":[{"name":"credentials","ok":false,"ms":2,"error":"no credentials"}],"error":"credentials: no credentials"}"#
        let c = controller(InMemorySecretStore(), calls: Calls()) { _ in CommandResult(status: 1, stdout: Data(json.utf8)) }
        let r = await c.test()
        XCTAssertEqual(try r.get().steps.first?.error, "no credentials")
    }

    func testProfilesMergeTheCLIsKinds() async throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("aws-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        try "[profile dev]\nsso_session = corp\n[default]\nregion = us-east-1\n".write(to: dir.appendingPathComponent("config"), atomically: true, encoding: .utf8)
        let c = controller(InMemorySecretStore(), calls: Calls()) { _ in
            CommandResult(status: 0, stdout: Data(#"[{"name":"default","kind":"process","region":"us-east-1"},{"name":"dev","kind":"sso"},{"name":"ci","kind":"static"}]"#.utf8))
        }
        let list = await c.profiles(home: URL(fileURLWithPath: "/none"), environment: ["AWS_CONFIG_FILE": dir.appendingPathComponent("config").path])
        XCTAssertEqual(list.map(\.name), ["default", "dev", "ci"])
        XCTAssertEqual(list[0].kind, "process")
        XCTAssertTrue(list[1].isSSO)
    }

    func testDatabaseStatusNotAvailable() async {
        let c = controller(InMemorySecretStore(), calls: Calls()) { _ in CommandResult(status: 2, stderr: Data("unknown command".utf8)) }
        let status = await c.databaseStatus()
        let backups = await c.baseBackups()
        XCTAssertNil(status)
        XCTAssertNil(backups)
    }
}
