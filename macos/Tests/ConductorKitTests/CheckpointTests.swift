import XCTest
@testable import ConductorKit

final class CheckpointTests: XCTestCase {
    /// The shape of internal/checkpoint.Manifest as `conductor checkpoint list --json` emits it.
    static let listJSON = """
    [
      {
        "schema": 1,
        "id": "20261007T101500Z-claude-9a474a",
        "created_at": "2026-10-07T10:15:00.123456Z",
        "machine": "ada-mac",
        "reason": "quota",
        "note": "Captured because this login reached its usage limit threshold.",
        "harness": "claude",
        "harness_version": "2.1.0",
        "session_id": "3f9c1e2a-0000-4000-8000-000000000001",
        "title": "Fix the flaky scheduler test",
        "cwd": "/Users/ada/src/conductor",
        "repo": {"root": "/Users/ada/src/conductor", "remote": "git@github.com:aburan28/conductor.git", "branch": "fix/sched", "head": "abc", "dirty": true},
        "conductor": {"project": "conductor", "session_id": "s-1", "task": "T-42"},
        "transcript": {"path": "native/x.jsonl", "bytes": 1234, "user_turns": 12, "assistant_turns": 30, "last_activity": "2026-10-07T10:14:00Z"},
        "workspace": {"patch_bytes": 99},
        "files": [{"path": "manifest.json", "size": 1, "sha256": "x"}]
      },
      {
        "schema": 1,
        "id": "20261006T090000Z-codex-11aa22",
        "created_at": "2026-10-06T09:00:00Z",
        "reason": "periodic",
        "harness": "codex",
        "session_id": "rollout-1",
        "cwd": "/Users/ada/src/site",
        "repo": {"dirty": false},
        "transcript": {"path": "native/r.jsonl", "bytes": 10},
        "workspace": {},
        "files": []
      },
      {
        "schema": 1,
        "id": "20261007T080000Z-claude-0b0b0b",
        "created_at": "2026-10-07T08:00:00Z",
        "reason": "periodic",
        "harness": "claude",
        "session_id": "3f9c1e2a-0000-4000-8000-000000000001",
        "cwd": "/Users/ada/src/conductor",
        "repo": {"dirty": false},
        "transcript": {"path": "native/x.jsonl", "bytes": 1000},
        "workspace": {},
        "files": []
      }
    ]
    """

    func testDecodesTheList() throws {
        let list = try CheckpointManifest.decodeList(Data(Self.listJSON.utf8))
        XCTAssertEqual(list.count, 3)
        let first = list[0]
        XCTAssertEqual(first.shortID, "9a474a")
        XCTAssertEqual(first.harness, "claude")
        XCTAssertTrue(first.isUsageLimit)
        XCTAssertEqual(first.turns, 42)
        XCTAssertEqual(first.displayTitle, "Fix the flaky scheduler test")
        XCTAssertEqual(first.conductor?.task, "T-42")
        XCTAssertEqual(first.repo?.branch, "fix/sched")
        XCTAssertNotNil(first.createdDate)
        XCTAssertEqual(list[1].displayTitle, "site")
        XCTAssertFalse(list[1].isUsageLimit)
        XCTAssertEqual(try CheckpointManifest.decodeList(Data("[]".utf8)), [])
    }

    func testGroupsBySessionNewestFirst() throws {
        let groups = CheckpointGroup.group(try CheckpointManifest.decodeList(Data(Self.listJSON.utf8)))
        XCTAssertEqual(groups.map(\.key), ["claude/3f9c1e2a-0000-4000-8000-000000000001", "codex/rollout-1"])
        XCTAssertEqual(groups[0].checkpoints.map(\.shortID), ["9a474a", "0b0b0b"])
        XCTAssertEqual(groups[0].latest.shortID, "9a474a")
    }

    func testResumeArguments() {
        XCTAssertEqual(ResumeTarget.here.arguments(checkpoint: "c1"), ["checkpoint", "resume", "c1"])
        XCTAssertEqual(ResumeTarget.account("work").arguments(checkpoint: "c1"), ["checkpoint", "resume", "c1", "--account", "work"])
        XCTAssertEqual(ResumeTarget.harness(.codex).arguments(checkpoint: "c1"), ["checkpoint", "resume", "c1", "--harness", "codex"])
        XCTAssertEqual(Harness.normalize(" Claude-Code "), .claude)
        XCTAssertEqual(Harness.normalize("open-code"), .opencode)
        XCTAssertNil(Harness.normalize("cursor"))
    }

    func testAccountsFromBothConventions() throws {
        let fm = FileManager.default
        let home = fm.temporaryDirectory.appendingPathComponent("home-\(UUID().uuidString)")
        defer { try? fm.removeItem(at: home) }
        for dir in [".claude-work", ".claude-personal", ".claude", ".codex-ci", ".conductor/accounts/claude/max2", ".conductor/accounts/claude/.hidden"] {
            try fm.createDirectory(at: home.appendingPathComponent(dir), withIntermediateDirectories: true)
        }
        try Data().write(to: home.appendingPathComponent(".claude-notadir"))
        let state = home.appendingPathComponent(".conductor")
        XCTAssertEqual(Accounts.list(harness: "claude", home: home, conductorState: state), ["max2", "personal", "work"])
        XCTAssertEqual(Accounts.list(harness: "codex", home: home, conductorState: state), ["ci"])
        XCTAssertEqual(Accounts.list(harness: "opencode", home: home, conductorState: state), [])
    }

    func testUsageLimitWatcherIgnoresWhatWasAlreadyThere() throws {
        let all = try CheckpointManifest.decodeList(Data(Self.listJSON.utf8))
        var w = UsageLimitWatcher()
        XCTAssertEqual(w.newLimits(in: Array(all.dropFirst())), [], "the first look records, never notifies")
        XCTAssertEqual(w.newLimits(in: all).map(\.shortID), ["9a474a"])
        XCTAssertEqual(w.newLimits(in: all), [], "once")
    }

    // MARK: - terminal

    func testShellQuoting() {
        XCTAssertEqual(TerminalLauncher.shellQuote("plain-word_1.2"), "plain-word_1.2")
        XCTAssertEqual(TerminalLauncher.shellQuote(""), "''")
        XCTAssertEqual(TerminalLauncher.shellQuote("a b"), "'a b'")
        XCTAssertEqual(TerminalLauncher.shellQuote("it's"), "'it'\\''s'")
        XCTAssertEqual(TerminalLauncher.shellCommand(cwd: "/Users/ada/my repo", argv: ["/Applications/Conductor.app/Contents/Resources/bin/conductor", "checkpoint", "resume", "c1"]),
                       "cd '/Users/ada/my repo' && exec /Applications/Conductor.app/Contents/Resources/bin/conductor checkpoint resume c1")
    }

    func testOsascriptEscapesQuotesAndBackslashes() {
        let args = TerminalLauncher.osascriptArguments(shellCommand: #"cd '/a "b"\c' && exec x"#)
        XCTAssertEqual(args, [
            "/usr/bin/osascript",
            "-e", #"tell application "Terminal" to do script "cd '/a \"b\"\\c' && exec x""#,
            "-e", #"tell application "Terminal" to activate"#,
        ])
    }

    func testConductorTerminalTemplate() {
        XCTAssertEqual(TerminalLauncher.launchArguments(cwd: "/r", argv: ["conductor", "resume"], environment: ["CONDUCTOR_TERMINAL": "wezterm start --cwd {cwd} -- sh -c {cmd}"]),
                       ["wezterm", "start", "--cwd", "/r", "--", "sh", "-c", "cd /r && exec conductor resume"])
        XCTAssertEqual(TerminalLauncher.launchArguments(cwd: nil, argv: ["x"], environment: ["CONDUCTOR_TERMINAL": "kitty"]),
                       ["kitty", "sh", "-c", "exec x"])
        XCTAssertEqual(TerminalLauncher.launchArguments(cwd: nil, argv: ["x"], environment: [:]).first, "/usr/bin/osascript")
    }
}
