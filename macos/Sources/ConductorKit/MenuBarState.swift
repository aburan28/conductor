import Foundation

/// The menu bar dot: green when all is clear, amber when a conflict or an offer waits on the
/// person, grey when the daemon is down (docs/MACOS_APP.md, Screens).
public enum StatusDot: String, Equatable, Sendable {
    case clear
    case attention
    case down

    public static func compute(daemonUp: Bool, status: StatusSummary?, offers: [Assignment]) -> StatusDot {
        guard daemonUp else { return .down }
        if !(status?.openConflicts.isEmpty ?? true) { return .attention }
        if offers.contains(where: \.isOffered) { return .attention }
        return .clear
    }

    public var help: String {
        switch self {
        case .clear: return "Conductor: all clear"
        case .attention: return "Conductor: a conflict or an offer is waiting"
        case .down: return "Conductor: the control plane is not running"
        }
    }
}

/// What an event on the stream means to the app.
public enum EventReaction: Equatable, Sendable {
    /// Refresh the status: tasks, conflicts, presence or offers changed.
    case refresh
    /// A login hit its usage limit: tell the person, and offer the Checkpoints window.
    case usageLimit(harness: String?, detail: String)
    /// Nothing the menu bar shows.
    case ignore

    /// Event types that change what the popover shows.
    static let refreshPrefixes = ["task.", "conflict.", "scope.", "lease.", "session.", "presence.", "attempt.stalled"]

    public static func classify(_ e: DomainEvent) -> EventReaction {
        if e.type == "quota.exhausted" {
            let harness = e.payload?["harness"]?.stringValue
            var detail = "A login hit its usage limit"
            if let harness, !harness.isEmpty { detail = "A \(harness) login hit its usage limit" }
            if let window = e.payload?["kind"]?.stringValue, !window.isEmpty { detail += " (\(window) window)" }
            if let resets = e.payload?["expires_at"]?.stringValue, let d = ISO8601.parse(resets) {
                let f = DateFormatter()
                f.dateStyle = .none
                f.timeStyle = .short
                detail += "; it resets at \(f.string(from: d))"
            }
            return .usageLimit(harness: harness, detail: detail + ".")
        }
        if refreshPrefixes.contains(where: { e.type.hasPrefix($0) }) { return .refresh }
        return .ignore
    }
}
