import XCTest
@testable import ConductorKit

final class PullRequestTests: XCTestCase {
    func testRowsJoinTasksWithTheLatestCheck() throws {
        let status = try JSONDecoder().decode(StatusSummary.self, from: Data("""
        {"project":"conductor",
         "active":[{"id":"t1","ref":"T-1","status":"in_progress","title":"Scheduler","branch":"fix/sched","pull_request_url":"https://github.com/a/b/pull/7","pull_request_state":"open"},
                   {"id":"t2","ref":"T-2","status":"in_progress","title":"No PR yet","branch":"feat/x"}],
         "ready":[{"id":"t3","ref":"T-3","status":"ready","title":"Docs","branch":"docs/y","pull_request_url":"https://github.com/a/b/pull/9","pull_request_state":"draft"}]}
        """.utf8))
        var checks = PullRequestChecks()
        XCTAssertFalse(checks.record(DomainEvent(type: "task.claimed")))
        XCTAssertTrue(checks.record(DomainEvent(id: "e", projectID: "p", aggregateType: "project", aggregateID: "p", type: "github.pr_checked",
                                                payload: .object(["branch": .string("fix/sched"), "commit_sha": .string("abc"),
                                                                  "outcome": .string("action_required"), "count": .number(2)]),
                                                occurredAt: nil)))
        let rows = checks.rows(status)
        XCTAssertEqual(rows.map(\.taskRef), ["T-1", "T-3"])
        XCTAssertEqual(rows[0].check, .init(outcome: "action_required", commit: "abc", overlaps: 2))
        XCTAssertEqual(rows[0].label, "action required")
        XCTAssertTrue(rows[0].needsAttention)
        XCTAssertEqual(rows[1].label, "draft")
        XCTAssertFalse(rows[1].needsAttention)
        XCTAssertEqual(checks.rows(nil), [])
    }
}
