import XCTest
@testable import ConductorKit

/// Against "JSON for the app" in docs/STORAGE.md (branch feat/db-archive), verbatim where
/// the document gives values.
final class DatabaseBackupTests: XCTestCase {
    static let statusJSON = """
    { "configured": true, "enabled": true, "archiving": true, "location": "s3://bucket/conductor",
      "sealed": true, "system_id": "7693956267215457548",
      "archive": { "system_id": "7693956267215457548", "archived": 1432, "last_wal": "00000001000000000000059A",
                   "last_at": "2026-10-07T12:00:01Z", "last_error": "", "last_error_wal": "",
                   "last_error_at": "", "last_base_backup": "20261007T030000Z",
                   "last_base_backup_at": "2026-10-07T03:01:12Z", "last_base_backup_error": "" },
      "lag_seconds": 42, "failing": false,
      "base_backups": { "count": 7, "latest_id": "20261007T030000Z", "latest_at": "2026-10-07T03:01:12Z" },
      "wal": { "segments": 1432, "bytes": 24025956352, "first": "000000010000000000000001", "last": "00000001000000000000059A" },
      "error": "" }
    """

    static let backupsJSON = """
    { "system_id": "7693956267215457548", "location": "s3://bucket/conductor",
      "backups": [ { "version": 1, "id": "20261006T030000Z", "system_id": "7693956267215457548", "pg_version": "17.2",
                     "started_at": "2026-10-06T03:00:00Z", "finished_at": "2026-10-06T03:01:10Z", "start_lsn": "0/2000028", "end_lsn": "0/2000100",
                     "timeline": 1, "start_wal": "000000010000000000000002", "wal_segment_size": 16777216,
                     "size": 40606720, "stored_size": 40642123, "sealed": true, "key_id": "k1" },
                   { "version": 1, "id": "20261007T030000Z", "started_at": "2026-10-07T03:00:00Z", "finished_at": "2026-10-07T03:01:12Z",
                     "size": 40700000, "sealed": true } ],
      "wal": { "segments": 12, "bytes": 201326592, "first": "a", "last": "b" } }
    """

    func testStatus() throws {
        let s = try XCTUnwrap(DatabaseStatus.decode(Data(Self.statusJSON.utf8)))
        XCTAssertEqual(s.archiving, true)
        XCTAssertEqual(s.archive?.archived, 1432)
        XCTAssertNil(s.archive?.lastError, "empty strings are no value")
        XCTAssertEqual(s.baseBackups?.count, 7)
        XCTAssertEqual(s.rows, [
            .init("Archiving", "on"),
            .init("Location", "s3://bucket/conductor"),
            .init("Sealed", "yes"),
            .init("Last archived segment", "00000001000000000000059A"),
            .init("Archived at", "2026-10-07T12:00:01Z"),
            .init("Segments archived", "1432"),
            .init("Lag", "42 s"),
            .init("Last base backup", "20261007T030000Z, 2026-10-07T03:01:12Z"),
            .init("Base backups in the bucket", "7"),
            .init("WAL in the bucket", "1432 segments, 22.4 GB"),
            .init("Cluster", "7693956267215457548"),
        ])
        XCTAssertNil(s.problem)
    }

    func testLocalStatusHasOnlyWhatThisMachineRecorded() throws {
        // --local: no base_backups and no wal; empty fields omitted.
        let json = #"{"configured":true,"enabled":true,"archiving":true,"archive":{"archived":3,"last_wal":"000000010000000000000003","last_base_backup":"20261007T030000Z"},"lag_seconds":3600}"#
        let s = try XCTUnwrap(DatabaseStatus.decode(Data(json.utf8)))
        XCTAssertNil(s.baseBackups)
        XCTAssertEqual(s.rows.map(\.label), ["Archiving", "Last archived segment", "Segments archived", "Lag", "Last base backup"])
        XCTAssertEqual(s.rows[3].value, "60 min")
    }

    func testStatusProblems() throws {
        let failing = try XCTUnwrap(DatabaseStatus.decode(Data(#"{"archiving":true,"failing":true,"archive":{"last_error":"AccessDenied","last_error_wal":"00000001000000000000000A","last_error_at":"2026-10-07T12:00:00Z"}}"#.utf8)))
        XCTAssertEqual(failing.problem, "Archiving failed for 00000001000000000000000A at 2026-10-07T12:00:00Z: AccessDenied")
        let off = try XCTUnwrap(DatabaseStatus.decode(Data(#"{"configured":false,"enabled":false,"archiving":false,"error":"no bucket is configured"}"#.utf8)))
        XCTAssertEqual(off.problem, "no bucket is configured")
        XCTAssertEqual(off.rows, [.init("Archiving", "off")])
    }

    func testUnknownFieldsStillShow() throws {
        let s = try XCTUnwrap(DatabaseStatus.decode(Data(#"{"archiving":true,"retention_days":14,"nested":{"x":1}}"#.utf8)))
        XCTAssertEqual(s.rows, [.init("Archiving", "on"), .init("Retention days", "14")])
    }

    func testStatusThatIsNotJSONIsNotAvailable() {
        XCTAssertNil(DatabaseStatus.decode(Data("conductor: unknown command \"db\"".utf8)))
        XCTAssertNil(DatabaseStatus.decode(Data("[1,2]".utf8)))
    }

    func testBackups() throws {
        let list = try XCTUnwrap(BaseBackupList.decode(Data(Self.backupsJSON.utf8)))
        XCTAssertEqual(list.count, 2)
        XCTAssertEqual(list.latest?.id, "20261007T030000Z")
        XCTAssertEqual(list.latest?.takenAt, "2026-10-07T03:01:12Z")
        XCTAssertEqual(list.backups[0].bytes, 40606720)
        XCTAssertNil(list.error)
    }

    func testNoClusterInTheBucket() throws {
        let list = try XCTUnwrap(BaseBackupList.decode(Data(#"{"backups":[],"error":"no cluster in the bucket"}"#.utf8)))
        XCTAssertTrue(list.isEmpty)
        XCTAssertEqual(list.error, "no cluster in the bucket")
        XCTAssertTrue(try XCTUnwrap(BaseBackupList.decode(Data(#"{"backups":null,"error":"x"}"#.utf8))).isEmpty)
        XCTAssertTrue(try XCTUnwrap(BaseBackupList.decode(Data("[]".utf8))).isEmpty)
        XCTAssertNil(BaseBackupList.decode(Data(#"{"something":"else"}"#.utf8)))
    }

    func testRestoreIsOfferedOnlyOnAFreshMacWithBackupsInTheBucket() throws {
        let show = try StorageShow.decode(Data(#"{"configured":true,"source":"file","s3":{"bucket":"b"},"uses":{"database":true}}"#.utf8))
        let some = BaseBackupList(backups: [.init(id: "a", takenAt: nil, bytes: nil)])
        let none = BaseBackupList(backups: [])
        XCTAssertTrue(RestoreOffer.shouldOffer(clusterExists: false, storage: show, backups: some))
        XCTAssertFalse(RestoreOffer.shouldOffer(clusterExists: true, storage: show, backups: some))
        XCTAssertFalse(RestoreOffer.shouldOffer(clusterExists: false, storage: show, backups: none))
        XCTAssertFalse(RestoreOffer.shouldOffer(clusterExists: false, storage: show, backups: nil))
        XCTAssertFalse(RestoreOffer.shouldOffer(clusterExists: false, storage: nil, backups: some))
        var off = show
        off.settings.uses.database = false
        XCTAssertFalse(RestoreOffer.shouldOffer(clusterExists: false, storage: off, backups: some))
    }

    func testStorageThatCannotBeReadIsNotNothingConfigured() throws {
        XCTAssertEqual(StorageReading.from(nil), .unreadable("the conductor command did not run"))
        guard case .unreadable(let why) = StorageReading.from(CommandResult(status: 1, stderr: Data("no keychain".utf8))) else {
            return XCTFail("a failed command is unreadable")
        }
        XCTAssertEqual(why, "no keychain")
        guard case .unreadable = StorageReading.from(CommandResult(status: 0, stdout: Data("not json".utf8))) else {
            return XCTFail("output that does not decode is unreadable")
        }
        // Nothing configured is a successful read, not a failure.
        let none = StorageReading.from(CommandResult(status: 0, stdout: Data(#"{"configured":false,"source":"none"}"#.utf8)))
        XCTAssertFalse(try XCTUnwrap(none.show).databaseToBucket)
    }

    func testNewClusterGate() throws {
        let bucket = try StorageShow.decode(Data(#"{"configured":true,"source":"file","s3":{"bucket":"b"},"uses":{"database":true}}"#.utf8))
        let none = try StorageShow.decode(Data(#"{"configured":false,"source":"none"}"#.utf8))
        let some = BaseBackupList(backups: [.init(id: "a", takenAt: nil, bytes: nil)])
        let empty = BaseBackupList(backups: [])
        // A cluster that exists is never created again.
        XCTAssertEqual(NewClusterGate.verdict(clusterExists: true, storage: .unreadable("x"), backups: nil), .create)
        // Nothing configured, or storage that does not go to a bucket: create.
        XCTAssertEqual(NewClusterGate.verdict(clusterExists: false, storage: .read(none), backups: nil), .create)
        // A bucket with an empty listing: create. With backups: offer the restore.
        XCTAssertEqual(NewClusterGate.verdict(clusterExists: false, storage: .read(bucket), backups: empty), .create)
        XCTAssertEqual(NewClusterGate.verdict(clusterExists: false, storage: .read(bucket), backups: some), .offerRestore(some))
        // Storage that cannot be read, or a bucket whose listing failed: refuse.
        XCTAssertTrue(isRefusal(NewClusterGate.verdict(clusterExists: false, storage: .unreadable("no keychain"), backups: nil)))
        XCTAssertTrue(isRefusal(NewClusterGate.verdict(clusterExists: false, storage: .read(bucket), backups: nil)))
    }

    private func isRefusal(_ verdict: NewClusterGate.Verdict) -> Bool {
        if case .refuse = verdict { return true }
        return false
    }

    func testJSONValueRoundTripsWholeNumbers() throws {
        let v = try JSONValue.decode(Data(#"{"a":24,"b":1.5,"c":[true,null,"x"]}"#.utf8))
        let again = try JSONValue.decode(try JSONEncoder().encode(v))
        XCTAssertEqual(v, again)
        XCTAssertEqual(String(decoding: try JSONEncoder().encode(JSONValue.number(24)), as: UTF8.self), "24")
        XCTAssertEqual(v.first("missing", "a")?.intValue, 24)
    }
}
