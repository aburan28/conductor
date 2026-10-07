import Foundation

// The database half of docs/STORAGE.md as the app reads it. `conductor db status --json` and
// `conductor db backups --json` are being written at the same time as this app, and the
// contract names their commands but not their output, so both are read leniently: as any
// JSON, picking out the fields under the names they are most likely to have, and showing
// whatever else is there as it is. Nothing here fails because a field is missing or new.

/// `conductor db status --json`, for the Database durability section.
public struct DatabaseStatus: Equatable, Sendable {
    public struct Row: Equatable, Identifiable, Sendable {
        public var label: String
        public var value: String
        public var id: String { label }

        public init(_ label: String, _ value: String) {
            self.label = label
            self.value = value
        }
    }

    /// The rows to show, the well-known fields first in a fixed order, then the rest.
    public var rows: [Row]
    /// A problem the status itself reports (a failing archive), if any field says so.
    public var problem: String?

    /// Field spellings tried for each well-known row, in order.
    static let known: [(label: String, keys: [String])] = [
        ("Archiving", ["archiving", "archive_mode", "archive_wal"]),
        ("Last archived segment", ["last_archived_segment", "last_archived_wal", "last_archived"]),
        ("Archived at", ["last_archived_at", "last_archived_time"]),
        ("Last base backup", ["last_base_backup", "latest_base_backup", "last_backup"]),
        ("Base backup taken", ["last_base_backup_at", "last_backup_at"]),
        ("Lag", ["lag_seconds", "lag", "archive_lag_seconds"]),
        ("Failed archive attempts", ["failed_count", "archive_failures"]),
        ("Last failure", ["last_failed_wal", "last_failed_at"]),
        ("Cluster", ["system_identifier", "system_id"]),
        ("Location", ["location", "bucket"]),
    ]

    public static func decode(_ data: Data) -> DatabaseStatus? {
        guard let value = try? JSONValue.decode(data), case .object(let object) = value else { return nil }
        var rows: [Row] = []
        var used = Set<String>()
        for (label, keys) in known {
            for key in keys {
                guard let v = object[key], v != .null else { continue }
                used.insert(key)
                rows.append(Row(label, render(v, key: key)))
                break
            }
        }
        for key in object.keys.sorted() where !used.contains(key) {
            guard let v = object[key], v != .null else { continue }
            if key == "error" || key == "problem" { continue }
            switch v {
            case .object, .array: continue // nested detail belongs to the CLI's own output
            default: rows.append(Row(humanize(key), v.displayText))
            }
        }
        var problem: String?
        for key in ["error", "problem", "last_error"] {
            if let s = object[key]?.stringValue, !s.isEmpty { problem = s; break }
        }
        return DatabaseStatus(rows: rows, problem: problem)
    }

    static func render(_ v: JSONValue, key: String) -> String {
        switch v {
        case .number(let n) where key.hasSuffix("seconds") || key == "lag":
            return formatSeconds(n)
        case .object(let o):
            // A base backup described as an object: its id and when it was taken.
            let id = JSONValue.object(o).first("id", "name", "label")?.displayText
            let at = JSONValue.object(o).first("taken_at", "created_at", "finished_at", "time", "timestamp")?.displayText
            return [id, at].compactMap { $0 }.joined(separator: ", ").ifEmpty(v.displayText)
        default:
            return v.displayText
        }
    }

    static func formatSeconds(_ s: Double) -> String {
        if s < 120 { return "\(Int(s.rounded())) s" }
        if s < 7200 { return "\(Int((s / 60).rounded())) min" }
        return String(format: "%.1f h", s / 3600)
    }

    static func humanize(_ key: String) -> String {
        let words = key.replacingOccurrences(of: "_", with: " ").replacingOccurrences(of: "-", with: " ")
        guard let first = words.first else { return key }
        return first.uppercased() + words.dropFirst()
    }
}

/// `conductor db backups --json`: the base backups in the bucket. Accepts a bare array or an
/// object holding one under `backups` (or `base_backups`).
public struct BaseBackupList: Equatable, Sendable {
    public struct Backup: Equatable, Identifiable, Sendable {
        public var id: String
        public var takenAt: String?
        public var bytes: Int?
    }

    public var backups: [Backup]

    public var isEmpty: Bool { backups.isEmpty }
    public var count: Int { backups.count }

    /// The newest, by the time each names (ISO 8601 sorts as text), else the last listed.
    public var latest: Backup? {
        let dated = backups.filter { $0.takenAt != nil }
        if !dated.isEmpty { return dated.max { ($0.takenAt ?? "") < ($1.takenAt ?? "") } }
        return backups.last
    }

    public static func decode(_ data: Data) -> BaseBackupList? {
        guard let value = try? JSONValue.decode(data) else { return nil }
        let items: [JSONValue]
        switch value {
        case .array(let a): items = a
        case .object:
            guard let a = value.first("backups", "base_backups")?.arrayValue else { return nil }
            items = a
        default: return nil
        }
        var out: [Backup] = []
        for (i, item) in items.enumerated() {
            switch item {
            case .string(let s):
                out.append(Backup(id: s, takenAt: nil, bytes: nil))
            case .object:
                let taken = item.first("taken_at", "created_at", "started_at", "finished_at", "time", "timestamp")?.stringValue
                let id = item.first("id", "name", "label", "timestamp")?.displayText ?? taken ?? "backup \(i + 1)"
                let bytes = item.first("bytes", "size", "size_bytes")?.intValue
                out.append(Backup(id: id, takenAt: taken, bytes: bytes))
            default:
                continue
            }
        }
        return BaseBackupList(backups: out)
    }
}

/// Whether to offer "Restore from bucket" on first run: there is no cluster on this Mac yet,
/// the storage settings send the database to a bucket, and that bucket holds base backups.
public enum RestoreOffer {
    public static func shouldOffer(clusterExists: Bool, storage: StorageShow?, backups: BaseBackupList?) -> Bool {
        guard !clusterExists, let storage, storage.databaseToBucket, let backups else { return false }
        return !backups.isEmpty
    }
}

extension String {
    func ifEmpty(_ other: @autoclosure () -> String) -> String { isEmpty ? other() : self }
}
