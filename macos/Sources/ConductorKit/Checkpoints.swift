import Foundation

/// One checkpoint as `conductor checkpoint list --json` prints it (internal/checkpoint
/// Manifest). Only what the Checkpoints window shows is read; the rest is left alone.
public struct CheckpointManifest: Decodable, Equatable, Identifiable, Sendable {
    public struct Repo: Decodable, Equatable, Sendable {
        public var root: String?
        public var remote: String?
        public var branch: String?
        public var dirty: Bool?
    }

    public struct Transcript: Decodable, Equatable, Sendable {
        public var userTurns: Int?
        public var assistantTurns: Int?
        public var lastActivity: String?

        enum CodingKeys: String, CodingKey {
            case userTurns = "user_turns"
            case assistantTurns = "assistant_turns"
            case lastActivity = "last_activity"
        }
    }

    public struct ConductorRef: Decodable, Equatable, Sendable {
        public var project: String?
        public var sessionID: String?
        public var task: String?

        enum CodingKeys: String, CodingKey {
            case project
            case sessionID = "session_id"
            case task
        }
    }

    public var id: String
    public var createdAt: String
    public var machine: String?
    public var reason: String?
    public var note: String?
    public var harness: String
    public var harnessVersion: String?
    public var sessionID: String
    public var title: String?
    public var cwd: String
    public var repo: Repo?
    public var conductor: ConductorRef?
    public var transcript: Transcript?

    enum CodingKeys: String, CodingKey {
        case id
        case createdAt = "created_at"
        case machine, reason, note, harness
        case harnessVersion = "harness_version"
        case sessionID = "session_id"
        case title, cwd, repo, conductor, transcript
    }

    public init(from decoder: Decoder) throws {
        let c = try decoder.container(keyedBy: CodingKeys.self)
        id = try c.decode(String.self, forKey: .id)
        createdAt = try c.decodeIfPresent(String.self, forKey: .createdAt) ?? ""
        machine = try c.decodeIfPresent(String.self, forKey: .machine)
        reason = try c.decodeIfPresent(String.self, forKey: .reason)
        note = try c.decodeIfPresent(String.self, forKey: .note)
        harness = try c.decodeIfPresent(String.self, forKey: .harness) ?? ""
        harnessVersion = try c.decodeIfPresent(String.self, forKey: .harnessVersion)
        sessionID = try c.decodeIfPresent(String.self, forKey: .sessionID) ?? ""
        title = try c.decodeIfPresent(String.self, forKey: .title)
        cwd = try c.decodeIfPresent(String.self, forKey: .cwd) ?? ""
        repo = try c.decodeIfPresent(Repo.self, forKey: .repo)
        conductor = try c.decodeIfPresent(ConductorRef.self, forKey: .conductor)
        transcript = try c.decodeIfPresent(Transcript.self, forKey: .transcript)
    }

    public static func decodeList(_ data: Data) throws -> [CheckpointManifest] {
        try JSONDecoder().decode([CheckpointManifest].self, from: data)
    }

    /// The short form the CLI prints (internal/checkpoint.ShortID): what follows the last
    /// dash of the id.
    public var shortID: String {
        if let dash = id.lastIndex(of: "-"), id.index(after: dash) < id.endIndex {
            return String(id[id.index(after: dash)...])
        }
        return id
    }

    /// The session a checkpoint belongs to: a harness and its own conversation id.
    public var sessionKey: String { harness + "/" + sessionID }

    /// Taken because the login ran out (cmd/conductor/wrapquota.go captures with reason
    /// "quota" when a session's own login reaches its limit).
    public var isUsageLimit: Bool { reason == "quota" }

    public var turns: Int { (transcript?.userTurns ?? 0) + (transcript?.assistantTurns ?? 0) }

    /// What a row is called: the harness's own title, else the folder.
    public var displayTitle: String {
        if let t = title, !t.isEmpty { return t }
        if !cwd.isEmpty { return (cwd as NSString).lastPathComponent }
        return sessionID
    }

    public var createdDate: Date? { ISO8601.parse(createdAt) }
}

/// The Checkpoints window's rows: one group per session, newest session first, each group's
/// checkpoints newest first.
public struct CheckpointGroup: Equatable, Identifiable, Sendable {
    public var key: String
    public var checkpoints: [CheckpointManifest]
    public var id: String { key }
    public var latest: CheckpointManifest { checkpoints[0] }

    public static func group(_ list: [CheckpointManifest]) -> [CheckpointGroup] {
        var order: [String] = []
        var byKey: [String: [CheckpointManifest]] = [:]
        for m in list {
            if byKey[m.sessionKey] == nil { order.append(m.sessionKey) }
            byKey[m.sessionKey, default: []].append(m)
        }
        let groups = order.map { key in
            CheckpointGroup(key: key, checkpoints: byKey[key]!.sorted { newer($0, than: $1) })
        }
        return groups.sorted { newer($0.latest, than: $1.latest) }
    }

    static func newer(_ a: CheckpointManifest, than b: CheckpointManifest) -> Bool {
        switch (a.createdDate, b.createdDate) {
        case let (x?, y?): return x > y
        default: return a.createdAt > b.createdAt
        }
    }
}

/// Which harnesses a checkpoint can continue in.
public enum Harness: String, CaseIterable, Identifiable, Sendable {
    case claude, codex, opencode

    public var id: String { rawValue }

    public var title: String {
        switch self {
        case .claude: return "Claude Code"
        case .codex: return "Codex"
        case .opencode: return "OpenCode"
        }
    }

    /// The CLI's own normalisation (internal/checkpoint NormalizeHarness): names it accepts.
    public static func normalize(_ s: String) -> Harness? {
        switch s.trimmingCharacters(in: .whitespaces).lowercased() {
        case "claude", "claude-code", "claudecode": return .claude
        case "codex", "codex-cli": return .codex
        case "opencode", "open-code": return .opencode
        default: return nil
        }
    }
}

/// How to continue a checkpoint.
public enum ResumeTarget: Equatable, Sendable {
    case here
    case account(String)
    case harness(Harness)

    /// `conductor checkpoint resume <id> [--account NAME | --harness H]`.
    public func arguments(checkpoint id: String) -> [String] {
        var args = ["checkpoint", "resume", id]
        switch self {
        case .here: break
        case .account(let name): args += ["--account", name]
        case .harness(let h): args += ["--harness", h.rawValue]
        }
        return args
    }
}

/// The other logins on this Mac a session could continue under: `~/.<harness>-<name>` (the
/// community convention for Claude Code, each with its own credentials) and
/// `~/.conductor/accounts/<harness>/<name>`, the two places `checkpoint resume --account`
/// looks (cmd/conductor/checkpoint.go accountStateDir).
public enum Accounts {
    public static func list(harness: String, home: URL, conductorState: URL,
                            fileManager: FileManager = .default) -> [String] {
        var names = Set<String>()
        let prefix = "." + harness + "-"
        if let entries = try? fileManager.contentsOfDirectory(atPath: home.path) {
            for e in entries where e.hasPrefix(prefix) && e.count > prefix.count {
                var isDir: ObjCBool = false
                if fileManager.fileExists(atPath: home.appendingPathComponent(e).path, isDirectory: &isDir), isDir.boolValue {
                    names.insert(String(e.dropFirst(prefix.count)))
                }
            }
        }
        let accounts = conductorState.appendingPathComponent("accounts").appendingPathComponent(harness)
        if let entries = try? fileManager.contentsOfDirectory(atPath: accounts.path) {
            for e in entries where !e.hasPrefix(".") {
                var isDir: ObjCBool = false
                if fileManager.fileExists(atPath: accounts.appendingPathComponent(e).path, isDirectory: &isDir), isDir.boolValue {
                    names.insert(e)
                }
            }
        }
        return names.sorted()
    }
}

/// Notices "a session hit its usage limit" from the checkpoint list: a checkpoint taken for
/// that reason that was not there last time. The first look only records what exists, so
/// opening the app does not replay old limits.
public struct UsageLimitWatcher: Sendable {
    private var seen: Set<String>?

    public init() {}

    public mutating func newLimits(in list: [CheckpointManifest]) -> [CheckpointManifest] {
        let ids = Set(list.map(\.id))
        defer { seen = (seen ?? []).union(ids) }
        guard let seen else { return [] }
        return list.filter { $0.isUsageLimit && !seen.contains($0.id) }
    }
}

/// RFC 3339 times as Go writes them (`time.RFC3339Nano`: up to nine fractional digits, which
/// not every ISO8601DateFormatter reads).
enum ISO8601 {
    static func parse(_ s: String) -> Date? {
        var text = s
        if let dot = text.firstIndex(of: "."), let tPos = text.firstIndex(of: "T"), dot > tPos {
            var end = text.index(after: dot)
            while end < text.endIndex, text[end].isNumber { end = text.index(after: end) }
            let digits = text[text.index(after: dot)..<end]
            let millis = String(digits.prefix(3)).padding(toLength: 3, withPad: "0", startingAt: 0)
            text = String(text[..<dot]) + "." + millis + String(text[end...])
        }
        let withFraction = ISO8601DateFormatter()
        withFraction.formatOptions = [.withInternetDateTime, .withFractionalSeconds]
        if let d = withFraction.date(from: text) { return d }
        let plain = ISO8601DateFormatter()
        plain.formatOptions = [.withInternetDateTime]
        return plain.date(from: text)
    }
}
