import XCTest
@testable import ConductorKit

final class AWSProfilesTests: XCTestCase {
    static let config = """
    # my AWS config
    [default]
    region = us-east-1
    output = json

    [profile dev-sso]
    sso_session = corp
    sso_account_id = 111122223333
    sso_role_name = Developer
    region = eu-west-1

    [ profile   legacy-sso ]
    sso_start_url = https://corp.awsapps.com/start
    sso_region = us-east-1

    [sso-session corp]
    sso_start_url = https://corp.awsapps.com/start
    sso_region = us-east-1

    [profile deploy]
    role_arn = arn:aws:iam::111122223333:role/deploy
    source_profile = default
    s3 =
      max_concurrent_requests = 20
      role_arn = should-not-count

    [profile tool]
    credential_process = /usr/local/bin/get-creds
    ; a comment
    [services local-minio]
    s3 =
      endpoint_url = http://localhost:9000
    """

    static let credentials = "[default]\r\naws_access_key_id = AKIADEFAULT\r\naws_secret_access_key = x\r\n\r\n[ci]\r\naws_access_key_id = AKIACI\r\naws_secret_access_key = y\r\n"

    func testParsesProfilesFromBothFiles() {
        let list = AWSProfiles.profiles(config: AWSProfiles.parseINI(Self.config), credentials: AWSProfiles.parseINI(Self.credentials))
        XCTAssertEqual(list.map(\.name), ["default", "ci", "deploy", "dev-sso", "legacy-sso", "tool"])
        let byName = Dictionary(uniqueKeysWithValues: list.map { ($0.name, $0) })
        XCTAssertEqual(byName["default"]?.kind, "static")
        XCTAssertEqual(byName["default"]?.region, "us-east-1")
        XCTAssertEqual(byName["default"]?.sources, ["config", "credentials"])
        XCTAssertEqual(byName["ci"]?.kind, "static")
        XCTAssertEqual(byName["ci"]?.sources, ["credentials"])
        XCTAssertEqual(byName["dev-sso"]?.kind, "sso")
        XCTAssertEqual(byName["dev-sso"]?.isSSO, true)
        XCTAssertEqual(byName["dev-sso"]?.region, "eu-west-1")
        XCTAssertEqual(byName["legacy-sso"]?.isSSO, true, "section names with extra spaces")
        XCTAssertEqual(byName["deploy"]?.kind, "assume-role")
        XCTAssertEqual(byName["deploy"]?.isSSO, false)
        XCTAssertEqual(byName["tool"]?.kind, "process")
        XCTAssertNil(byName["corp"], "an sso-session section is not a profile")
        XCTAssertNil(byName["local-minio"], "a services section is not a profile")
    }

    func testIndentedNestedSettingsAreSkipped() {
        let ini = AWSProfiles.parseINI(Self.config)
        XCTAssertEqual(ini["profile deploy"]?["role_arn"], "arn:aws:iam::111122223333:role/deploy")
        XCTAssertNil(ini["profile deploy"]?["max_concurrent_requests"])
        XCTAssertEqual(ini["profile deploy"]?["s3"], "")
    }

    func testFilesHonourTheEnvironment() {
        let home = URL(fileURLWithPath: "/Users/ada")
        var f = AWSProfiles.files(home: home, environment: [:])
        XCTAssertEqual(f.config.path, "/Users/ada/.aws/config")
        XCTAssertEqual(f.credentials.path, "/Users/ada/.aws/credentials")
        f = AWSProfiles.files(home: home, environment: ["AWS_CONFIG_FILE": "/etc/aws/config", "AWS_SHARED_CREDENTIALS_FILE": "/etc/aws/creds"])
        XCTAssertEqual(f.config.path, "/etc/aws/config")
        XCTAssertEqual(f.credentials.path, "/etc/aws/creds")
    }

    func testLoadsFromDisk() throws {
        let dir = FileManager.default.temporaryDirectory.appendingPathComponent("aws-\(UUID().uuidString)")
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }
        try Self.config.write(to: dir.appendingPathComponent("cfg"), atomically: true, encoding: .utf8)
        let list = AWSProfiles.load(home: URL(fileURLWithPath: "/nonexistent"),
                                    environment: ["AWS_CONFIG_FILE": dir.appendingPathComponent("cfg").path])
        XCTAssertEqual(list.first?.name, "default")
        XCTAssertTrue(list.contains { $0.name == "dev-sso" && $0.isSSO })
        XCTAssertEqual(AWSProfiles.load(home: URL(fileURLWithPath: "/nonexistent"), environment: [:]), [])
    }

    func testSSOLoginAndCLILookup() {
        XCTAssertEqual(AWSProfiles.ssoLoginArguments(profile: "dev-sso"), ["sso", "login", "--profile", "dev-sso"])
        XCTAssertEqual(AWSProfiles.locateCLI(environment: ["PATH": "/x:/y"], isExecutable: { $0 == "/y/aws" })?.path, "/y/aws")
        XCTAssertEqual(AWSProfiles.locateCLI(environment: [:], isExecutable: { $0 == "/usr/local/bin/aws" })?.path, "/usr/local/bin/aws")
        XCTAssertNil(AWSProfiles.locateCLI(environment: ["PATH": "/x"], isExecutable: { _ in false }))
    }
}
