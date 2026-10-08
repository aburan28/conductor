import Foundation
#if canImport(Darwin)
import Darwin
#elseif canImport(Glibc)
import Glibc
#elseif canImport(Musl)
import Musl
#endif

/// What a finished command left behind.
public struct CommandResult: Equatable, Sendable {
    public var status: Int32
    public var stdout: Data
    public var stderr: Data

    public init(status: Int32, stdout: Data = Data(), stderr: Data = Data()) {
        self.status = status
        self.stdout = stdout
        self.stderr = stderr
    }

    public var succeeded: Bool { status == 0 }
    public var stdoutText: String { String(decoding: stdout, as: UTF8.self) }
    public var stderrText: String { String(decoding: stderr, as: UTF8.self) }

    /// What to tell a person when the command failed: its own words on stderr when it said
    /// any, else stdout, else the status.
    public func failureMessage(_ what: String) -> String {
        let err = stderrText.trimmingCharacters(in: .whitespacesAndNewlines)
        if !err.isEmpty { return err }
        let out = stdoutText.trimmingCharacters(in: .whitespacesAndNewlines)
        if !out.isEmpty { return out }
        return "\(what) exited with status \(status)"
    }
}

/// One command to run: the executable, its arguments, and what it runs with.
public struct CommandSpec: Equatable, Sendable {
    public var executable: URL
    public var arguments: [String]
    /// nil inherits this process's environment.
    public var environment: [String: String]?
    public var currentDirectory: URL?
    /// Written to the command's standard input, which is then closed. Secrets travel this
    /// way and never on the command line, where any local user can read them with `ps`.
    public var stdin: Data?

    public init(_ executable: URL, _ arguments: [String] = [], environment: [String: String]? = nil,
                currentDirectory: URL? = nil, stdin: Data? = nil) {
        self.executable = executable
        self.arguments = arguments
        self.environment = environment
        self.currentDirectory = currentDirectory
        self.stdin = stdin
    }
}

public enum CommandError: Error, CustomStringConvertible, Equatable {
    case launch(String, String)

    public var description: String {
        switch self {
        case .launch(let path, let why): return "could not start \(path): \(why)"
        }
    }
}

/// Runs commands. A protocol so the supervisor and the settings model can be tested with a
/// recorder instead of a machine.
public protocol CommandRunning: Sendable {
    func run(_ spec: CommandSpec) async throws -> CommandResult
}

/// The real thing, on `Process`.
public struct ProcessRunner: CommandRunning {
    public init() {}

    public func run(_ spec: CommandSpec) async throws -> CommandResult {
        try await withCheckedThrowingContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                do {
                    continuation.resume(returning: try Self.runBlocking(spec))
                } catch {
                    continuation.resume(throwing: error)
                }
            }
        }
    }

    /// Runs and waits on the calling thread. stdout and stderr are drained on their own
    /// threads while stdin is written, so a command that fills one pipe while the other is
    /// being read, or that writes a lot before reading its input, cannot deadlock.
    public static func runBlocking(_ spec: CommandSpec) throws -> CommandResult {
        let p = Process()
        p.executableURL = spec.executable
        p.arguments = spec.arguments
        if let env = spec.environment { p.environment = env }
        if let dir = spec.currentDirectory { p.currentDirectoryURL = dir }
        let out = Pipe(), err = Pipe()
        p.standardOutput = out
        p.standardError = err
        let input: Pipe?
        if spec.stdin != nil {
            input = Pipe()
            p.standardInput = input
        } else {
            input = nil
            p.standardInput = FileHandle.nullDevice
        }
        do {
            try p.run()
        } catch {
            throw CommandError.launch(spec.executable.path, error.localizedDescription)
        }

        let group = DispatchGroup()
        let collected = Collected()
        group.enter()
        DispatchQueue.global().async {
            collected.setOut(out.fileHandleForReading.readDataToEndOfFile())
            group.leave()
        }
        group.enter()
        DispatchQueue.global().async {
            collected.setErr(err.fileHandleForReading.readDataToEndOfFile())
            group.leave()
        }
        if let input, let data = spec.stdin {
            // A command that exits without reading its input closes the pipe; writing to it
            // then raises SIGPIPE, which Foundation turns into an exception on some
            // platforms. Ignore it for this write: the exit status says what happened.
            let writer = input.fileHandleForWriting
            ignoreSIGPIPE()
            writeAll(data, to: writer.fileDescriptor)
            try? writer.close()
        }
        p.waitUntilExit()
        group.wait()
        return CommandResult(status: p.terminationStatus, stdout: collected.out, stderr: collected.err)
    }

    private static func writeAll(_ data: Data, to fd: Int32) {
        data.withUnsafeBytes { (raw: UnsafeRawBufferPointer) in
            guard let base = raw.baseAddress else { return }
            var offset = 0
            while offset < raw.count {
                let n = write(fd, base + offset, raw.count - offset)
                if n > 0 {
                    offset += n
                } else if n < 0 && errno == EINTR {
                    continue
                } else {
                    return
                }
            }
        }
    }

    private static let sigpipeOnce: Void = {
        signal(SIGPIPE, SIG_IGN)
    }()

    private static func ignoreSIGPIPE() { _ = sigpipeOnce }

    private final class Collected: @unchecked Sendable {
        private let lock = NSLock()
        private var _out = Data(), _err = Data()
        func setOut(_ d: Data) { lock.lock(); _out = d; lock.unlock() }
        func setErr(_ d: Data) { lock.lock(); _err = d; lock.unlock() }
        var out: Data { lock.lock(); defer { lock.unlock() }; return _out }
        var err: Data { lock.lock(); defer { lock.unlock() }; return _err }
    }
}

/// A runner for tests: records every command and answers from a script.
public final class RecordingRunner: CommandRunning, @unchecked Sendable {
    private let lock = NSLock()
    private var _commands: [CommandSpec] = []
    private let respond: @Sendable (CommandSpec) -> CommandResult

    public init(respond: @escaping @Sendable (CommandSpec) -> CommandResult = { _ in CommandResult(status: 0) }) {
        self.respond = respond
    }

    public var commands: [CommandSpec] {
        lock.lock(); defer { lock.unlock() }
        return _commands
    }

    /// Each command as one line: the executable's name and its arguments.
    public var transcript: [String] {
        commands.map { ([$0.executable.lastPathComponent] + $0.arguments).joined(separator: " ") }
    }

    public func run(_ spec: CommandSpec) async throws -> CommandResult {
        record(spec)
        return respond(spec)
    }

    private func record(_ spec: CommandSpec) {
        lock.lock()
        defer { lock.unlock() }
        _commands.append(spec)
    }
}
