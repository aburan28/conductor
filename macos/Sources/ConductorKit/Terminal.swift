import Foundation

/// Opening a command in a terminal window, the way `conductor resume` does it on a Mac
/// (internal/localstate/terminal.go): `$CONDUCTOR_TERMINAL` when the person chose a terminal,
/// otherwise a new Terminal.app window through `osascript`. A resumed session needs a window
/// a person can type in, not a background process.
public enum TerminalLauncher {
    /// The argv to run: the chosen terminal's, or osascript's.
    public static func launchArguments(cwd: String?, argv: [String], environment: [String: String]) -> [String] {
        let shell = shellCommand(cwd: cwd, argv: argv)
        if let template = environment["CONDUCTOR_TERMINAL"], !template.isEmpty {
            let expanded = expandTemplate(template, shellCommand: shell, cwd: cwd ?? "")
            if !expanded.isEmpty { return expanded }
        }
        return osascriptArguments(shellCommand: shell)
    }

    /// `cd <cwd> && exec <argv…>`, every word quoted for /bin/sh.
    public static func shellCommand(cwd: String?, argv: [String]) -> String {
        let command = "exec " + argv.map(shellQuote).joined(separator: " ")
        if let cwd, !cwd.isEmpty { return "cd " + shellQuote(cwd) + " && " + command }
        return command
    }

    /// Single quotes around anything a shell would read specially; plain words as they are.
    public static func shellQuote(_ s: String) -> String {
        if s.isEmpty { return "''" }
        let special = CharacterSet(charactersIn: " \t\n\"'\\$&|;<>()*?[]#~`!{}")
        if s.rangeOfCharacter(from: special) == nil { return s }
        return "'" + s.replacingOccurrences(of: "'", with: "'\\''") + "'"
    }

    /// `osascript -e 'tell application "Terminal" to do script "…"' -e '… activate'`.
    public static func osascriptArguments(shellCommand: String, osascript: String = "/usr/bin/osascript") -> [String] {
        let escaped = shellCommand.replacingOccurrences(of: "\\", with: "\\\\").replacingOccurrences(of: "\"", with: "\\\"")
        return [
            osascript,
            "-e", "tell application \"Terminal\" to do script \"\(escaped)\"",
            "-e", "tell application \"Terminal\" to activate",
        ]
    }

    /// A `CONDUCTOR_TERMINAL` value as an argv: `{cmd}` and `{cwd}` substituted per field,
    /// never re-split; without `{cmd}`, `sh -c <cmd>` is appended.
    public static func expandTemplate(_ template: String, shellCommand: String, cwd: String) -> [String] {
        var out: [String] = []
        var sawCommand = false
        for field in template.split(whereSeparator: { $0 == " " || $0 == "\t" || $0 == "\n" }) {
            let f = String(field)
            if f.contains("{cmd}") { sawCommand = true }
            let expanded = f.replacingOccurrences(of: "{cmd}", with: shellCommand).replacingOccurrences(of: "{cwd}", with: cwd)
            if expanded.trimmingCharacters(in: .whitespaces).isEmpty { continue }
            out.append(expanded)
        }
        if !out.isEmpty && !sawCommand { out += ["sh", "-c", shellCommand] }
        return out
    }
}
