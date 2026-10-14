import XCTest
@testable import ConductorKit

/// Against the JSON in docs/STORAGE.md, verbatim.
final class StorageTests: XCTestCase {
    static let settingsJSON = """
    {
      "version": 1,
      "s3": {
        "bucket": "my-team-conductor",
        "region": "us-east-1",
        "endpoint": "",
        "path_style": false,
        "insecure": false,
        "prefix": "conductor"
      },
      "auth": {
        "method": "static",
        "access_key_id": "AKIA...",
        "secret": "keychain",
        "secret_access_key": "",
        "profile": ""
      },
      "uses": { "sessions": true, "checkpoints": true, "database": true },
      "database": {
        "archive_wal": true,
        "archive_timeout_seconds": 60,
        "base_backup_every_hours": 24,
        "keep_base_backups": 7,
        "seal": true
      }
    }
    """

    static let showJSON = """
    { "configured": true, "path": "/Users/me/.conductor/storage.json", "source": "file",
      "s3": { "bucket": "my-team-conductor", "region": "eu-west-1", "endpoint": "https://minio.lan:9000",
              "path_style": true, "insecure": false, "prefix": "team" },
      "auth": { "method": "static", "access_key_id": "AKIA...", "secret": "keychain",
      "secret_access_key": "", "profile": "" },
      "uses": { "sessions": true, "checkpoints": false, "database": true },
      "database": { "archive_wal": true, "archive_timeout_seconds": 60, "base_backup_every_hours": 12,
                    "keep_base_backups": 5, "seal": false },
      "effective_region": "eu-west-1", "auth_description": "access key AKIA..., secret in the Keychain" }
    """

    static let testJSON = """
    { "ok": true, "credentials": "static (keychain)", "location": "s3://bucket/prefix",
      "steps": [ { "name": "credentials", "ok": true, "ms": 3 },
                 { "name": "put", "ok": true, "ms": 41 },
                 { "name": "get", "ok": true, "ms": 22 },
                 { "name": "list", "ok": true, "ms": 30 },
                 { "name": "delete", "ok": true, "ms": 25 } ],
      "error": "" }
    """

    func testDecodesTheSettingsFile() throws {
        let s = try StorageSettings.decode(Data(Self.settingsJSON.utf8))
        XCTAssertEqual(s.version, 1)
        XCTAssertEqual(s.s3, .init(bucket: "my-team-conductor", region: "us-east-1", endpoint: "", pathStyle: false, insecure: false, prefix: "conductor"))
        XCTAssertEqual(s.auth.method, .static)
        XCTAssertEqual(s.auth.accessKeyID, "AKIA...")
        XCTAssertEqual(s.auth.secret, "keychain")
        XCTAssertEqual(s.uses, .init(sessions: true, checkpoints: true, database: true))
        XCTAssertEqual(s.database, .init(archiveWAL: true, archiveTimeoutSeconds: 60, baseBackupEveryHours: 24, keepBaseBackups: 7, seal: true))
    }

    func testEncodesTheSameFields() throws {
        let s = try StorageSettings.decode(Data(Self.settingsJSON.utf8))
        let encoded = try JSONEncoder().encode(s)
        XCTAssertEqual(try StorageSettings.decode(encoded), s)
        let original = try JSONValue.decode(Data(Self.settingsJSON.utf8))
        XCTAssertEqual(try JSONValue.decode(encoded), original, "every field of the documented file, and nothing else")
    }

    func testMissingFieldsTakeTheCLIDefaults() throws {
        // What the CLI writes when uses and database were never set (omitempty).
        let s = try StorageSettings.decode(Data(#"{"version":1,"s3":{"bucket":"b"},"auth":{"method":"profile","profile":"dev"},"uses":{"checkpoints":false},"database":{"keep_base_backups":0}}"#.utf8))
        XCTAssertEqual(s.uses, .init(sessions: true, checkpoints: false, database: true))
        XCTAssertEqual(s.database, .init())
        XCTAssertEqual(s.effectivePrefix, "conductor")
        XCTAssertEqual(s.auth.method, .profile)
        XCTAssertEqual(s.auth.profile, "dev")
    }

    func testDecodesShow() throws {
        let show = try StorageShow.decode(Data(Self.showJSON.utf8))
        XCTAssertTrue(show.configured)
        XCTAssertFalse(show.off)
        XCTAssertEqual(show.source, "file")
        XCTAssertEqual(show.path, "/Users/me/.conductor/storage.json")
        XCTAssertEqual(show.effectiveRegion, "eu-west-1")
        XCTAssertEqual(show.authDescription, "access key AKIA..., secret in the Keychain")
        XCTAssertEqual(show.settings.s3.prefix, "team")
        XCTAssertTrue(show.settings.s3.pathStyle)
        XCTAssertFalse(show.settings.uses.checkpoints)
        XCTAssertEqual(show.settings.database.baseBackupEveryHours, 12)
        XCTAssertTrue(show.databaseToBucket)
        XCTAssertFalse(show.overriddenByEnvironment)
    }

    func testShowWhenNothingIsConfigured() throws {
        let show = try StorageShow.decode(Data(#"{"configured":false,"path":"/Users/me/.conductor/storage.json","source":"none","s3":{"bucket":"","region":"","endpoint":"","path_style":false,"insecure":false,"prefix":""},"auth":{"method":"","access_key_id":"","secret":"","secret_access_key":"","profile":""},"uses":{"checkpoints":true,"database":true,"sessions":true},"database":{"archive_wal":true,"archive_timeout_seconds":60,"base_backup_every_hours":24,"keep_base_backups":7,"seal":true}}"#.utf8))
        XCTAssertFalse(show.configured)
        XCTAssertFalse(show.databaseToBucket)
        let form = StorageForm(show)
        XCTAssertEqual(form.method, .static, "a fresh pane offers an access key")
        XCTAssertEqual(form.bucket, "")
    }

    func testOffAndEnvironmentOverride() throws {
        let show = try StorageShow.decode(Data(#"{"configured":true,"off":true,"source":"env","s3":{"bucket":"b"}}"#.utf8))
        XCTAssertTrue(show.off)
        XCTAssertTrue(show.overriddenByEnvironment)
        XCTAssertFalse(show.databaseToBucket)
    }

    func testDecodesTest() throws {
        let result = try StorageTestResult.from(CommandResult(status: 0, stdout: Data(Self.testJSON.utf8))).get()
        XCTAssertTrue(result.ok)
        XCTAssertEqual(result.credentials, "static (keychain)")
        XCTAssertEqual(result.location, "s3://bucket/prefix")
        XCTAssertEqual(result.steps.map(\.name), ["credentials", "put", "get", "list", "delete"])
        XCTAssertEqual(result.steps.map(\.ms), [3, 41, 22, 30, 25])
        XCTAssertNil(result.error)
    }

    func testAFailedTestStillPrintsItsSteps() throws {
        // `storage test --json` exits non-zero when ok is false, with the JSON on stdout.
        let json = #"{"ok":false,"credentials":"profile dev (sso)","location":"s3://b/conductor","steps":[{"name":"credentials","ok":true,"ms":120},{"name":"put","ok":false,"ms":88,"error":"AccessDenied: not authorized to PutObject"}],"error":"put: AccessDenied"}"#
        let result = try StorageTestResult.from(CommandResult(status: 1, stdout: Data(json.utf8), stderr: Data("conductor: storage test failed".utf8))).get()
        XCTAssertFalse(result.ok)
        XCTAssertEqual(result.steps[1].error, "AccessDenied: not authorized to PutObject")
        XCTAssertNil(result.steps[0].error)
        XCTAssertEqual(result.error, "put: AccessDenied")
    }

    func testATestThatPrintedNothing() {
        let r = StorageTestResult.from(CommandResult(status: 2, stderr: Data("conductor: unknown command storage\n".utf8)))
        XCTAssertEqual(r, .failure(.command("conductor: unknown command storage")))
    }

    func testProfilesFromTheCLI() throws {
        let list = try XCTUnwrap(CLIProfile.decodeList(Data(#"[{"name":"default","kind":"static","region":"us-east-1"},{"name":"dev","kind":"sso"}]"#.utf8)))
        XCTAssertEqual(list, [CLIProfile(name: "default", kind: "static", region: "us-east-1"), CLIProfile(name: "dev", kind: "sso", region: nil)])
    }

    // MARK: - storage set

    private func form() -> StorageForm {
        var f = StorageForm()
        f.bucket = " my-team-conductor "
        f.region = "us-east-1"
        f.prefix = "conductor"
        f.accessKeyID = "AKIAEXAMPLE"
        return f
    }

    func testSetWithTheSecretAlreadyInTheKeychain() {
        XCTAssertEqual(form().setArguments(secret: .keychain), [
            "storage", "set", "--json",
            "--bucket=my-team-conductor", "--region=us-east-1", "--prefix=conductor", "--endpoint=",
            "--path-style=false", "--insecure=false",
            "--sessions=true", "--checkpoints=true", "--database=true",
            "--archive-wal=true", "--archive-timeout=60", "--base-backup-every=24", "--keep=7", "--seal=true",
            "--auth=static", "--access-key-id=AKIAEXAMPLE", "--secret-from=keychain",
        ])
    }

    func testSetWithTheSecretOnStandardInput() {
        var f = form()
        f.newSecret = "  wJalrXUtnFEMI/K7MDENG  "
        let args = f.setArguments(secret: .stdin)
        XCTAssertEqual(Array(args.suffix(4)), ["--auth=static", "--access-key-id=AKIAEXAMPLE", "--secret-from=stdin", "--secret-store=keychain"])
        XCTAssertFalse(args.joined(separator: " ").contains("wJalr"), "the secret is never an argument")
        XCTAssertEqual(f.secretStdin, Data("wJalrXUtnFEMI/K7MDENG\n".utf8))
    }

    func testEveryFlagUsesEquals() {
        var f = form()
        f.endpoint = "-http://weird"
        for arg in f.setArguments(secret: .keychain).dropFirst(3) {
            XCTAssertTrue(arg.hasPrefix("--") && arg.contains("="), "\(arg) must be --name=value")
        }
    }

    func testSetWithAProfile() {
        var f = form()
        f.method = .profile
        f.profile = "dev-sso"
        f.endpoint = "https://minio.lan:9000"
        f.pathStyle = true
        f.database = false
        let args = f.setArguments(secret: .keychain)
        XCTAssertTrue(args.contains("--endpoint=https://minio.lan:9000"))
        XCTAssertTrue(args.contains("--path-style=true"))
        XCTAssertTrue(args.contains("--database=false"))
        XCTAssertEqual(Array(args.suffix(2)), ["--auth=profile", "--profile=dev-sso"])
        XCTAssertFalse(args.contains { $0.hasPrefix("--access-key-id") || $0.hasPrefix("--secret") })
    }

    func testSetWithTheEnvironment() {
        var f = form()
        f.method = .environment
        XCTAssertEqual(f.setArguments(secret: .keychain).last, "--auth=environment")
    }

    func testDatabaseFlags() {
        var f = form()
        f.archiveWAL = false
        f.archiveTimeoutSeconds = 30
        f.baseBackupEveryHours = 6
        f.keepBaseBackups = 3
        f.seal = false
        let args = f.setArguments(secret: .keychain)
        for expected in ["--archive-wal=false", "--archive-timeout=30", "--base-backup-every=6", "--keep=3", "--seal=false"] {
            XCTAssertTrue(args.contains(expected), expected)
        }
    }

    func testFormFromShowRoundTrips() throws {
        let show = try StorageShow.decode(Data(Self.showJSON.utf8))
        let f = StorageForm(show)
        XCTAssertEqual(f.bucket, "my-team-conductor")
        XCTAssertEqual(f.endpoint, "https://minio.lan:9000")
        XCTAssertTrue(f.usesCustomEndpoint)
        XCTAssertEqual(f.method, .static)
        XCTAssertEqual(f.newSecret, "")
        XCTAssertFalse(f.checkpoints)
        XCTAssertFalse(f.seal)
        XCTAssertEqual(f.keepBaseBackups, 5)
    }

    func testProblems() {
        var f = StorageForm()
        XCTAssertEqual(f.problems(secretInKeychain: false), ["Enter the bucket name.", "Enter the access key ID.", "Enter the secret access key."])
        f.bucket = "b"
        f.accessKeyID = "AKIA"
        XCTAssertEqual(f.problems(secretInKeychain: true), [])
        f.endpoint = "http://minio.lan:9000"
        XCTAssertEqual(f.problems(secretInKeychain: true), ["The endpoint is plain http. Turn on \"Allow plain HTTP\" or use https."])
        f.insecure = true
        XCTAssertEqual(f.problems(secretInKeychain: true), [])
        f.endpoint = "minio.lan"
        XCTAssertEqual(f.problems(secretInKeychain: true).count, 1)
        f.endpoint = ""
        f.method = .profile
        f.keepBaseBackups = 0
        XCTAssertEqual(f.problems(secretInKeychain: false), ["Keep at least one base backup."])
    }
}
