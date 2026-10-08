// A Keychain access list that names the applications allowed to read an item without asking.
//
// The secrets this app stores for the `conductor` command (the S3 secret access key, the
// seal passphrase) are read by that command through `/usr/bin/security
// find-generic-password -w` (docs/STORAGE.md). An item created the ordinary way trusts only
// its creator, so every read by the CLI, including the ones Postgres makes through
// `archive_command` while nobody is looking, would put up a password prompt. The way to say
// "this app and /usr/bin/security" is a SecAccess built from SecTrustedApplication values.
//
// Those APIs were deprecated in macOS 10.10 and have no replacement for the file-based
// login keychain: the data-protection keychain has access groups instead, which a
// command-line tool outside this app's team cannot join. They still work. This target exists
// so the deprecation warnings stay here: it is compiled with -suppress-warnings (see
// Package.swift) and holds nothing else.
#if canImport(Security) && os(macOS)
import Foundation
import Security

public enum TrustedAccessError: Error, CustomStringConvertible {
    case trustedApplication(path: String, status: OSStatus)
    case access(status: OSStatus)

    public var description: String {
        switch self {
        case .trustedApplication(let path, let status):
            return "could not name \(path) as a trusted application (OSStatus \(status))"
        case .access(let status):
            return "could not create the keychain access list (OSStatus \(status))"
        }
    }
}

public enum TrustedAccess {
    /// A SecAccess whose access list trusts the calling application (when `includeSelf`) and
    /// every executable in `paths`, labelled `label` (the name Keychain Access shows when it
    /// asks about anything else).
    public static func make(label: String, includeSelf: Bool = true, paths: [String]) throws -> SecAccess {
        var trusted: [SecTrustedApplication] = []
        if includeSelf {
            var me: SecTrustedApplication?
            let status = SecTrustedApplicationCreateFromPath(nil, &me)
            guard status == errSecSuccess, let me else {
                throw TrustedAccessError.trustedApplication(path: "this application", status: status)
            }
            trusted.append(me)
        }
        for path in paths {
            var app: SecTrustedApplication?
            let status = path.withCString { SecTrustedApplicationCreateFromPath($0, &app) }
            guard status == errSecSuccess, let app else {
                throw TrustedAccessError.trustedApplication(path: path, status: status)
            }
            trusted.append(app)
        }
        var access: SecAccess?
        let status = SecAccessCreate(label as CFString, trusted as CFArray, &access)
        guard status == errSecSuccess, let access else {
            throw TrustedAccessError.access(status: status)
        }
        return access
    }
}
#endif
