import Foundation

// The database half of docs/STORAGE.md as the app reads it: `conductor db status --json`
// and `conductor db backups --json` ("JSON for the app"). Both are read as any JSON and
// picked apart field by field: every field may be missing ("empty fields are omitted"), and
// fields this app does not know are shown as they are rather than failing the decode.

/// `conductor db status --json [--local]`, for the Database durability section.
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

    /// What this machine recorded about archiving (`archive`); present offline (`--local`).
    public struct Archive: Equatable, Sendable {
        public var archived: Int?
        public var lastWAL: String?
        public var lastAt: String?
        public var lastError: String?
        public var lastErrorWAL: String?
        public var lastErrorAt: String?
        public var lastBaseBackup: String?
        public var lastBaseBackupAt: String?
        public var lastBaseBackupError: String?
    }

    /// What the bucket holds (`base_backups`); absent with `--local`.
    public struct BaseBackups: Equatable, Sendable {
        public var count: Int?
        public var latestID: String?
        public var latestAt: String?
    }

    /// The WAL in the bucket (`wal`).
    public struct WAL: Equatable, Sendable {
        public var segments: Int?
        public var bytes: Int?
    }

    public var configured: Bool?
    public var enabled: Bool?
    public var archiving: Bool?
    public var location: String?
    public var sealed: Bool?
    public var systemID: String?
    public var archive: Archive?
    public var lagSeconds: Double?
    public var failing: Bool?
    public var baseBackups: BaseBackups?
    public var wal: WAL?
    public var error: String?

    /// Every field this app does not read, as label and value, so a newer CLI's additions
    /// still show.
    public var extra: [Row] = []

    static let knownKeys: Set<String> = [
        "configured", "enabled", "archiving", "location", "sealed", "system_id", "archive",
        "lag_seconds", "failing", "base_backups", "wal", "error",
    ]

    public static func decode(_ data: Data) -> DatabaseStatus? {
        guard let v = try? JSONValue.decode(data), case .object(let o) = v else { return nil }
        func str(_ x: JSONValue?, _ k: String) -> String? {
            guard let s = x?[k]?.stringValue, !s.isEmpty else { return nil }
            return s
        }
        var s = DatabaseStatus()
        s.configured = v["configured"]?.boolValue
        s.enabled = v["enabled"]?.boolValue
        s.archiving = v["archiving"]?.boolValue
        s.location = str(v, "location")
        s.sealed = v["sealed"]?.boolValue
        s.systemID = str(v, "system_id")
        if let a = v["archive"], a.objectValue != nil {
            s.archive = Archive(archived: a["archived"]?.intValue, lastWAL: str(a, "last_wal"), lastAt: str(a, "last_at"),
                                lastError: str(a, "last_error"), lastErrorWAL: str(a, "last_error_wal"),
                                lastErrorAt: str(a, "last_error_at"), lastBaseBackup: str(a, "last_base_backup"),
                                lastBaseBackupAt: str(a, "last_base_backup_at"),
                                lastBaseBackupError: str(a, "last_base_backup_error"))
        }
        s.lagSeconds = v["lag_seconds"]?.doubleValue
        s.failing = v["failing"]?.boolValue
        if let b = v["base_backups"], b.objectValue != nil {
            s.baseBackups = BaseBackups(count: b["count"]?.intValue, latestID: str(b, "latest_id"), latestAt: str(b, "latest_at"))
        }
        if let w = v["wal"], w.objectValue != nil {
            s.wal = WAL(segments: w["segments"]?.intValue, bytes: w["bytes"]?.intValue)
        }
        s.error = str(v, "error")
        for key in o.keys.sorted() where !knownKeys.contains(key) {
            guard let value = o[key], value != .null else { continue }
            switch value {
            case .object, .array: continue
            default: s.extra.append(Row(humanize(key), value.displayText))
            }
        }
        return s
    }

    /// The rows the pane shows, in a fixed order, then anything new.
    public var rows: [Row] {
        var out: [Row] = []
        if let archiving { out.append(Row("Archiving", archiving ? "on" : "off")) }
        if let location { out.append(Row("Location", location)) }
        if let sealed { out.append(Row("Sealed", sealed ? "yes" : "no")) }
        if let a = archive {
            if let w = a.lastWAL { out.append(Row("Last archived segment", w)) }
            if let at = a.lastAt { out.append(Row("Archived at", at)) }
            if let n = a.archived { out.append(Row("Segments archived", String(n))) }
        }
        if let lag = lagSeconds { out.append(Row("Lag", Self.formatSeconds(lag))) }
        let lastID = baseBackups?.latestID ?? archive?.lastBaseBackup
        let lastAt = baseBackups?.latestAt ?? archive?.lastBaseBackupAt
        if lastID != nil || lastAt != nil {
            out.append(Row("Last base backup", [lastID, lastAt].compactMap { $0 }.joined(separator: ", ")))
        }
        if let n = baseBackups?.count { out.append(Row("Base backups in the bucket", String(n))) }
        if let w = wal, let n = w.segments {
            var text = "\(n) segment" + (n == 1 ? "" : "s")
            if let b = w.bytes { text += ", " + Self.formatBytes(b) }
            out.append(Row("WAL in the bucket", text))
        }
        if let id = systemID { out.append(Row("Cluster", id)) }
        return out + extra
    }

    /// What is wrong, in a sentence, when anything is.
    public var problem: String? {
        if let error { return error }
        if let e = archive?.lastError {
            var text = "Archiving failed"
            if let w = archive?.lastErrorWAL { text += " for \(w)" }
            if let at = archive?.lastErrorAt { text += " at \(at)" }
            return text + ": " + e
        }
        if let e = archive?.lastBaseBackupError { return "The last base backup failed: " + e }
        if failing == true { return "Archiving is failing." }
        return nil
    }

    static func formatSeconds(_ s: Double) -> String {
        if s < 120 { return "\(Int(s.rounded())) s" }
        if s < 7200 { return "\(Int((s / 60).rounded())) min" }
        return String(format: "%.1f h", s / 3600)
    }

    static func formatBytes(_ b: Int) -> String {
        let units = ["bytes", "KB", "MB", "GB", "TB"]
        var value = Double(b)
        var unit = 0
        while value >= 1024 && unit < units.count - 1 {
            value /= 1024
            unit += 1
        }
        return unit == 0 ? "\(b) bytes" : String(format: "%.1f ", value) + units[unit]
    }

    static func humanize(_ key: String) -> String {
        let words = key.replacingOccurrences(of: "_", with: " ").replacingOccurrences(of: "-", with: " ")
        guard let first = words.first else { return key }
        return first.uppercased() + words.dropFirst()
    }
}

/// `conductor db backups --json`: `{"system_id", "location", "backups": [...], "wal", "error"}`.
/// A bare array is accepted too.
public struct BaseBackupList: Equatable, Sendable {
    public struct Backup: Equatable, Identifiable, Sendable {
        public var id: String
        public var takenAt: String?
        public var bytes: Int?
    }

    public var backups: [Backup]
    /// Why the list is empty, when the CLI says ("no cluster in the bucket").
    public var error: String?

    public init(backups: [Backup], error: String? = nil) {
        self.backups = backups
        self.error = error
    }

    public var isEmpty: Bool { backups.isEmpty }
    public var count: Int { backups.count }

    /// The newest, by the time each names (RFC 3339 sorts as text), else the last listed.
    public var latest: Backup? {
        let dated = backups.filter { $0.takenAt != nil }
        if !dated.isEmpty { return dated.max { ($0.takenAt ?? "") < ($1.takenAt ?? "") } }
        return backups.last
    }

    public static func decode(_ data: Data) -> BaseBackupList? {
        guard let value = try? JSONValue.decode(data) else { return nil }
        let items: [JSONValue]
        var error: String?
        switch value {
        case .array(let a): items = a
        case .object:
            error = value["error"]?.stringValue.flatMap { $0.isEmpty ? nil : $0 }
            if let a = value.first("backups", "base_backups")?.arrayValue {
                items = a
            } else if value["backups"] == .null || error != nil {
                items = []
            } else {
                return nil
            }
        default: return nil
        }
        var out: [Backup] = []
        for (i, item) in items.enumerated() {
            switch item {
            case .string(let s):
                out.append(Backup(id: s, takenAt: nil, bytes: nil))
            case .object:
                let taken = item.first("finished_at", "started_at", "taken_at", "created_at")?.stringValue
                let id = item.first("id", "name")?.displayText ?? taken ?? "backup \(i + 1)"
                let bytes = item.first("size", "bytes", "stored_size")?.intValue
                out.append(Backup(id: id, takenAt: taken, bytes: bytes))
            default:
                continue
            }
        }
        return BaseBackupList(backups: out, error: error)
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

/// What `conductor storage show --json` told a start about storage. Nothing configured is a
/// successful read with `configured: false`. A failed command, or output that does not decode,
/// is unreadable: that says nothing about whether a bucket holds the database.
public enum StorageReading: Equatable, Sendable {
    case read(StorageShow)
    case unreadable(String)

    public static func from(_ result: CommandResult?) -> StorageReading {
        guard let result else { return .unreadable("the conductor command did not run") }
        guard result.succeeded else { return .unreadable(result.failureMessage("conductor storage show")) }
        guard let show = try? StorageShow.decode(result.stdout) else {
            return .unreadable("conductor storage show printed output that is not storage settings")
        }
        return .read(show)
    }

    /// The settings, when they were read.
    public var show: StorageShow? {
        if case .read(let show) = self { return show }
        return nil
    }
}

/// The one decision before a new database cluster is created (`initdb`). Launch, restart and
/// applying storage all ask it. A cluster is refused while this Mac's storage cannot be read,
/// and while the database goes to a bucket whose backups cannot be listed: either could hide
/// the database a restore needs.
public enum NewClusterGate {
    public enum Verdict: Equatable, Sendable {
        case create
        case offerRestore(BaseBackupList)
        case refuse(String)
    }

    /// `backups` is the bucket's listing, consulted only when storage sends the database to a
    /// bucket. nil there means the listing failed.
    public static func verdict(clusterExists: Bool, storage: StorageReading, backups: BaseBackupList?) -> Verdict {
        guard !clusterExists else { return .create }
        switch storage {
        case .unreadable(let why):
            return .refuse("Conductor could not read the storage settings (\(why)), so it will not create a new database. Check Settings → Storage, then try again.")
        case .read(let show):
            guard show.databaseToBucket else { return .create }
            guard let backups else {
                return .refuse("Conductor could not list the backups in the storage bucket, so it will not create a new database over them. Check Settings → Storage, then try again.")
            }
            if RestoreOffer.shouldOffer(clusterExists: false, storage: show, backups: backups) {
                return .offerRestore(backups)
            }
            return .create
        }
    }
}
