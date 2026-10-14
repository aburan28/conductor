import Foundation

/// One profile from the person's AWS files, for the Storage pane's picker.
public struct AWSProfile: Equatable, Identifiable, Sendable {
    public var name: String
    /// `static`, `sso`, `assume-role`, `process` or `unknown`, decided the way the CLI's
    /// `conductor storage profiles` decides it, so the two agree.
    public var kind: String
    public var region: String?
    /// The profile signs in through IAM Identity Center: it has `sso_start_url` or
    /// `sso_session`, and `aws sso login --profile <name>` refreshes it.
    public var isSSO: Bool
    /// Where the profile was found: "config", "credentials", or both.
    public var sources: [String]

    public var id: String { name }

    public var kindLabel: String {
        switch kind {
        case "static": return "access keys"
        case "sso": return "SSO"
        case "assume-role": return isSSO ? "SSO, assumed role" : "assumed role"
        case "process": return "credential process"
        default: return "no credentials found"
        }
    }
}

/// `~/.aws/config` and `~/.aws/credentials`, read the way the `conductor` CLI reads them
/// (internal/awscreds): `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` name the files
/// when set; `[default]` and `[profile NAME]` are profiles in config, every section is one in
/// credentials, and credentials override config key by key.
public enum AWSProfiles {
    /// An INI file: section → key (lowercased) → value.
    public typealias INI = [String: [String: String]]

    public static func files(home: URL, environment: [String: String]) -> (config: URL, credentials: URL) {
        let config = environment["AWS_CONFIG_FILE"].flatMap { $0.isEmpty ? nil : URL(fileURLWithPath: $0) }
            ?? home.appendingPathComponent(".aws/config")
        let credentials = environment["AWS_SHARED_CREDENTIALS_FILE"].flatMap { $0.isEmpty ? nil : URL(fileURLWithPath: $0) }
            ?? home.appendingPathComponent(".aws/credentials")
        return (config, credentials)
    }

    /// Every profile in the person's files, `default` first, then by name.
    public static func load(home: URL, environment: [String: String]) -> [AWSProfile] {
        let (configURL, credentialsURL) = files(home: home, environment: environment)
        let config = (try? String(contentsOf: configURL, encoding: .utf8)).map(parseINI) ?? [:]
        let credentials = (try? String(contentsOf: credentialsURL, encoding: .utf8)).map(parseINI) ?? [:]
        return profiles(config: config, credentials: credentials)
    }

    /// Lines: `[section]` headers with whitespace inside collapsed (`[ profile  dev ]` is
    /// `profile dev`), `key = value` pairs, comments starting `#` or `;`, and indented lines
    /// (nested settings under a key such as `s3 =`) skipped.
    public static func parseINI(_ text: String) -> INI {
        var out: INI = [:]
        var section: String?
        for rawSub in text.split(omittingEmptySubsequences: false, whereSeparator: { $0 == "\n" || $0 == "\r\n" }) {
            var raw = String(rawSub)
            if raw.hasSuffix("\r") { raw.removeLast() }
            let line = raw.trimmingCharacters(in: .whitespaces)
            if line.isEmpty || line.hasPrefix("#") || line.hasPrefix(";") { continue }
            if line.hasPrefix("[") && line.hasSuffix("]") {
                let inner = line.dropFirst().dropLast()
                let name = inner.split(whereSeparator: { $0 == " " || $0 == "\t" }).joined(separator: " ")
                section = name
                if out[name] == nil { out[name] = [:] }
                continue
            }
            guard let current = section, let first = raw.first, first != " ", first != "\t" else { continue }
            guard let eq = line.firstIndex(of: "=") else { continue }
            let key = line[..<eq].trimmingCharacters(in: .whitespaces).lowercased()
            let value = line[line.index(after: eq)...].trimmingCharacters(in: .whitespaces)
            out[current, default: [:]][key] = value
        }
        return out
    }

    public static func profiles(config: INI, credentials: INI) -> [AWSProfile] {
        var names: [String: [String]] = [:]
        for section in config.keys {
            if section == "default" {
                names["default", default: []].append("config")
            } else if section.hasPrefix("profile ") {
                let name = String(section.dropFirst("profile ".count))
                if !name.isEmpty { names[name, default: []].append("config") }
            }
        }
        for section in credentials.keys where !section.isEmpty {
            names[section, default: []].append("credentials")
        }
        let list = names.map { name, sources -> AWSProfile in
            let merged = merge(name, config: config, credentials: credentials)
            let region = merged["region"].flatMap { $0.isEmpty ? nil : $0 }
            let sso = !(merged["sso_start_url"] ?? "").isEmpty || !(merged["sso_session"] ?? "").isEmpty
            return AWSProfile(name: name, kind: kind(merged), region: region, isSSO: sso, sources: sources)
        }
        return list.sorted { a, b in
            if a.name == "default" { return b.name != "default" }
            if b.name == "default" { return false }
            return a.name < b.name
        }
    }

    /// A profile's settings: config's `[default]` (for `default`) and `[profile NAME]`, then
    /// credentials' `[NAME]` over them.
    public static func merge(_ name: String, config: INI, credentials: INI) -> [String: String] {
        var merged: [String: String] = [:]
        if name == "default", let s = config["default"] { merged.merge(s) { _, new in new } }
        if let s = config["profile \(name)"] { merged.merge(s) { _, new in new } }
        if let s = credentials[name] { merged.merge(s) { _, new in new } }
        return merged
    }

    /// The CLI's own classification (internal/awscreds.profileKind), in the same order.
    public static func kind(_ p: [String: String]) -> String {
        func has(_ k: String) -> Bool { !(p[k] ?? "").isEmpty }
        if has("role_arn") { return "assume-role" }
        if has("sso_session") || has("sso_start_url") { return "sso" }
        if has("credential_process") { return "process" }
        if has("aws_access_key_id") { return "static" }
        return "unknown"
    }

    /// The `aws` command, if installed: where the AWS installer and Homebrew put it, then
    /// the PATH.
    public static func locateCLI(environment: [String: String],
                                 isExecutable: (String) -> Bool = { FileManager.default.isExecutableFile(atPath: $0) }) -> URL? {
        var dirs = ["/usr/local/bin", "/opt/homebrew/bin", "/usr/local/aws-cli"]
        dirs += (environment["PATH"] ?? "").split(separator: ":").map(String.init)
        for dir in dirs where !dir.isEmpty {
            let path = (dir as NSString).appendingPathComponent("aws")
            if isExecutable(path) { return URL(fileURLWithPath: path) }
        }
        return nil
    }

    /// `aws sso login --profile NAME`.
    public static func ssoLoginArguments(profile: String) -> [String] {
        ["sso", "login", "--profile", profile]
    }
}
