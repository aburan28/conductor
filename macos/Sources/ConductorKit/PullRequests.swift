import Foundation

/// The popover's pull requests: each open task's pull request, with the outcome of the latest
/// Conductor check on its branch. Tasks carry the pull request (`pull_request_url`,
/// `pull_request_state`); the check arrives as a `github.pr_checked` event on the stream,
/// whose payload names the branch, the commit, and the outcome (internal/api/github.go).
public struct PullRequestChecks: Equatable, Sendable {
    public struct Check: Equatable, Sendable {
        public var outcome: String
        public var commit: String?
        public var overlaps: Int?
    }

    public struct Row: Equatable, Identifiable, Sendable {
        public var taskRef: String
        public var title: String?
        public var url: String
        public var state: String?
        public var check: Check?
        public var id: String { url }

        /// One word for the row: the check's outcome when there is one, else the PR's state.
        public var label: String {
            if let check { return check.outcome.replacingOccurrences(of: "_", with: " ") }
            return state ?? "open"
        }

        /// Whether the check says the pull request collides with someone's open work.
        public var needsAttention: Bool {
            guard let outcome = check?.outcome else { return false }
            return !["success", "neutral", "skipped"].contains(outcome)
        }
    }

    private var byBranch: [String: Check] = [:]

    public init() {}

    /// Takes a `github.pr_checked` event; anything else is ignored. Returns whether it was one.
    @discardableResult
    public mutating func record(_ e: DomainEvent) -> Bool {
        guard e.type == "github.pr_checked", let branch = e.payload?["branch"]?.stringValue, !branch.isEmpty,
              let outcome = e.payload?["outcome"]?.stringValue else { return false }
        byBranch[branch] = Check(outcome: outcome, commit: e.payload?["commit_sha"]?.stringValue,
                                 overlaps: e.payload?["count"]?.intValue)
        return true
    }

    public func check(branch: String) -> Check? { byBranch[branch] }

    /// The rows for a status: active tasks first, then ready ones, each with a pull request.
    public func rows(_ status: StatusSummary?) -> [Row] {
        let tasks = (status?.active ?? []) + (status?.ready ?? [])
        var seen = Set<String>()
        return tasks.compactMap { t in
            guard let url = t.pullRequestURL, !url.isEmpty, seen.insert(url).inserted else { return nil }
            return Row(taskRef: t.ref ?? t.id, title: t.title, url: url, state: t.pullRequestState,
                       check: t.branch.flatMap { byBranch[$0] })
        }
    }
}
