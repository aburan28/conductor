import Foundation

/// `launchctl` in the person's GUI domain (`gui/<uid>`), through a command runner so tests
/// can read back what would have run.
public struct Launchctl: Sendable {
    public static let path = URL(fileURLWithPath: "/bin/launchctl")
    public var uid: UInt32
    public var runner: CommandRunning

    public init(uid: UInt32, runner: CommandRunning) {
        self.uid = uid
        self.runner = runner
    }

    public var domain: String { "gui/\(uid)" }
    public func target(_ label: String) -> String { "\(domain)/\(label)" }

    private func run(_ args: [String]) async throws -> CommandResult {
        try await runner.run(CommandSpec(Self.path, args))
    }

    /// Loads a plist. A job already loaded under the label keeps its old definition until it
    /// is booted out, so that comes first; its failure (nothing was loaded) is expected.
    public func replace(label: String, plist: URL) async throws {
        _ = try await run(["bootout", target(label)])
        let result = try await run(["bootstrap", domain, plist.path])
        guard result.succeeded else {
            throw SupervisorError.launchctl("bootstrap \(label)", result.failureMessage("launchctl bootstrap"))
        }
    }

    /// Stops and unloads a job; the plist stays where it is.
    public func bootout(label: String) async throws {
        let result = try await run(["bootout", target(label)])
        // 3 (no such process) and 113 (not loaded) mean it was not running: fine.
        if !result.succeeded && ![3, 36, 113].contains(result.status) {
            if try await isLoaded(label: label) {
                throw SupervisorError.launchctl("bootout \(label)", result.failureMessage("launchctl bootout"))
            }
        }
    }

    public func isLoaded(label: String) async throws -> Bool {
        try await run(["print", target(label)]).succeeded
    }

    /// Runs a loaded job now (`-k` restarts one that is running).
    public func kickstart(label: String, restart: Bool = false) async throws {
        let args = restart ? ["kickstart", "-k", target(label)] : ["kickstart", target(label)]
        let result = try await run(args)
        guard result.succeeded else {
            throw SupervisorError.launchctl("kickstart \(label)", result.failureMessage("launchctl kickstart"))
        }
    }
}
