import Foundation

// The shapes conductord answers with, as far as the app reads them. Every field that the
// server may leave out is optional, so a newer server does not break an older app.

/// `GET /v1/local/status` (internal/api/local.go).
public struct LocalStatus: Codable, Equatable, Sendable {
    public var securityMode: String
    public var modeSource: String?
    public var localLoginAvailable: Bool
    public var reason: String?

    enum CodingKeys: String, CodingKey {
        case securityMode = "security_mode"
        case modeSource = "mode_source"
        case localLoginAvailable = "local_login_available"
        case reason
    }

    public init(securityMode: String, modeSource: String? = nil, localLoginAvailable: Bool, reason: String? = nil) {
        self.securityMode = securityMode
        self.modeSource = modeSource
        self.localLoginAvailable = localLoginAvailable
        self.reason = reason
    }

    /// The server is up but nobody owns this machine yet: `conductord bootstrap` has not run
    /// against this database. Local sign-in becomes possible once it has.
    public var needsOwner: Bool {
        !localLoginAvailable && securityMode == "local" && (reason ?? "").contains("no machine owner")
    }
}

/// `POST /v1/local/session` → 201.
public struct LocalSession: Codable, Equatable, Sendable {
    public var token: String
    public var handle: String?
    public var principalID: String?
    public var expiresAt: String?

    enum CodingKeys: String, CodingKey {
        case token, handle
        case principalID = "principal_id"
        case expiresAt = "expires_at"
    }
}

/// The error body every failed request carries.
public struct APIErrorBody: Codable, Equatable, Sendable {
    public var error: String
    public var code: String?
}

/// `GET /v1/whoami`.
public struct WhoAmI: Codable, Equatable, Sendable {
    public struct Principal: Codable, Equatable, Sendable {
        public var id: String?
        public var handle: String
    }

    public struct Project: Codable, Equatable, Sendable {
        public var id: String?
        public var slug: String
        public var role: String?
    }

    public var principal: Principal
    public var projects: [Project]
}

/// One live session: `domain.PresenceEntry`.
public struct PresenceEntry: Codable, Equatable, Identifiable, Sendable {
    public var sessionID: String
    public var principal: String
    public var kind: String?
    public var harness: String?
    public var state: String?
    public var taskRef: String?
    public var taskTitle: String?
    public var branch: String?
    public var scopes: [String]?
    public var lastHeartbeat: String?

    public var id: String { sessionID }

    enum CodingKeys: String, CodingKey {
        case sessionID = "session_id"
        case principal, kind, harness, state
        case taskRef = "task_ref"
        case taskTitle = "task_title"
        case branch, scopes
        case lastHeartbeat = "last_heartbeat"
    }
}

/// One side of a conflict.
public struct ConflictParty: Codable, Equatable, Sendable {
    public var taskID: String?
    public var taskRef: String?
    public var title: String?
    public var owner: String?

    enum CodingKeys: String, CodingKey {
        case taskID = "task_id"
        case taskRef = "task_ref"
        case title, owner
    }
}

/// `privacy.ConflictView`.
public struct ConflictView: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var kind: String?
    public var severity: String?
    /// The coordinator's suggestion: `suggest_join`, `suggest_wait`, `suggest_split`, …
    public var suggestion: String?
    public var state: String?
    public var resources: [String]?
    public var reason: String?
    public var other: ConflictParty?
    public var mine: ConflictParty?

    /// "join", "wait", "split", or the raw outcome for anything else.
    public var suggestionLabel: String {
        guard let s = suggestion, !s.isEmpty else { return "review" }
        return s.hasPrefix("suggest_") ? String(s.dropFirst("suggest_".count)) : s.replacingOccurrences(of: "_", with: " ")
    }
}

/// A task as the compact status lists it: only what the popover shows.
public struct TaskSummary: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var ref: String?
    public var status: String?
    public var owner: String?
    public var title: String?
    public var branch: String?
    public var pullRequestURL: String?
    public var pullRequestState: String?

    enum CodingKeys: String, CodingKey {
        case id, ref, status, owner, title, branch
        case pullRequestURL = "pull_request_url"
        case pullRequestState = "pull_request_state"
    }
}

/// `GET /v1/projects/{p}/status`: `coord.StatusSummary`.
public struct StatusSummary: Codable, Equatable, Sendable {
    public var project: String?
    public var counts: [String: Int]?
    public var active: [TaskSummary]?
    public var ready: [TaskSummary]?
    public var conflicts: [ConflictView]?
    public var presence: [PresenceEntry]?

    /// Conflicts still waiting on someone: not resolved, not ignored.
    public var openConflicts: [ConflictView] {
        (conflicts ?? []).filter { c in
            let s = c.state ?? "open"
            return s != "resolved" && s != "ignored"
        }
    }

    public var liveSessions: [PresenceEntry] { presence ?? [] }

    /// The sessions a principal (by handle) is running here: whose offers are theirs to answer.
    public func sessions(of handle: String) -> [PresenceEntry] {
        liveSessions.filter { $0.principal == handle }
    }
}

/// `domain.Assignment`: work offered to a session.
public struct Assignment: Codable, Equatable, Identifiable, Sendable {
    public var id: String
    public var taskID: String?
    public var taskRef: String?
    public var sessionID: String?
    public var state: String
    public var rationale: String?
    public var expiresAt: String?

    enum CodingKeys: String, CodingKey {
        case id
        case taskID = "task_id"
        case taskRef = "task_ref"
        case sessionID = "session_id"
        case state, rationale
        case expiresAt = "expires_at"
    }

    public var isOffered: Bool { state == "offered" }
}

/// `{"assignments": [...]}`.
public struct AssignmentList: Codable, Equatable, Sendable {
    public var assignments: [Assignment]
}

/// A domain event as the stream carries it in `data:` (`domain.Event`).
public struct DomainEvent: Codable, Equatable, Sendable {
    public var id: String?
    public var projectID: String?
    public var aggregateType: String?
    public var aggregateID: String?
    public var type: String
    public var payload: JSONValue?
    public var occurredAt: String?

    enum CodingKeys: String, CodingKey {
        case id
        case projectID = "project_id"
        case aggregateType = "aggregate_type"
        case aggregateID = "aggregate_id"
        case type, payload
        case occurredAt = "occurred_at"
    }

    public static func decode(_ event: SSEEvent) -> DomainEvent? {
        guard let data = event.data.data(using: .utf8),
              var e = try? JSONDecoder().decode(DomainEvent.self, from: data) else { return nil }
        if e.type.isEmpty, let name = event.event { e.type = name }
        return e
    }
}

/// `POST /v1/projects/{p}/members` (what `conductor invite` sends and reads).
public struct InviteRequest: Codable, Equatable, Sendable {
    public var handle: String
    public var role: String
    public var kind: String
    /// A Go duration ("168h"); nil lets the server apply its default.
    public var tokenTTL: String?

    enum CodingKeys: String, CodingKey {
        case handle, role, kind
        case tokenTTL = "token_ttl"
    }

    public init(handle: String, role: String, tokenTTL: String?) {
        self.handle = handle
        self.role = role
        self.kind = role == "runner" ? "runner_service" : "human"
        self.tokenTTL = tokenTTL
    }
}

public struct InviteResult: Codable, Equatable, Sendable {
    public var handle: String
    public var role: String
    public var token: String?
    public var expiresAt: String?
    public var createdPrincipal: Bool?
    public var existingPrincipal: Bool?
    public var note: String?

    enum CodingKeys: String, CodingKey {
        case handle, role, token, note
        case expiresAt = "expires_at"
        case createdPrincipal = "created_principal"
        case existingPrincipal = "existing_principal"
    }
}
