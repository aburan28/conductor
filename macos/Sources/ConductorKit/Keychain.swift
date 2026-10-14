import Foundation
#if canImport(Security)
import Security
#endif
#if canImport(Security) && os(macOS)
import ConductorKeychainACL
#endif

/// The Keychain items the app and the CLI share (docs/MACOS_APP.md, docs/STORAGE.md).
public enum KeychainNames {
    /// The app's own sign-in token; account is the control plane's endpoint.
    public static let tokenService = "dev.conductor"
    /// An S3 secret access key; account is the access key ID.
    public static let s3Service = "dev.conductor.s3"
    /// The passphrase that seals uploads; account is `default`.
    public static let sealService = "dev.conductor.seal"
    public static let sealAccount = "default"
    /// The DSN of a database the person runs themselves (Settings → attach to an existing
    /// database), which may hold a password; account is `default`.
    public static let databaseService = "dev.conductor.database"
    public static let databaseAccount = "default"
    /// What the CLI reads secrets through, and so what every shared item trusts.
    public static let securityTool = "/usr/bin/security"
}

public enum SecretStoreError: Error, Equatable, CustomStringConvertible {
    case status(String, Int32)
    case unavailable
    case access(String)

    public var description: String {
        switch self {
        case .status(let what, let code): return "\(what) failed (OSStatus \(code))"
        case .unavailable: return "the Keychain is only available on macOS"
        case .access(let why): return why
        }
    }
}

/// Generic passwords: what the app keeps in the Keychain. A protocol so everything that
/// stores a secret can be tested without one.
public protocol SecretStore: Sendable {
    func read(service: String, account: String) throws -> String?
    /// Whether an item exists, without reading its secret (and so without a prompt).
    func exists(service: String, account: String) throws -> Bool
    /// Creates or replaces the item. `trustedPaths` non-empty makes an item that this app
    /// and each of those executables can read without asking; empty, one only this app can.
    func write(_ secret: String, service: String, account: String, label: String, trustedPaths: [String]) throws
    func delete(service: String, account: String) throws
}

extension SecretStore {
    /// An item the `conductor` CLI can read through /usr/bin/security without a prompt.
    public func writeShared(_ secret: String, service: String, account: String, label: String) throws {
        try write(secret, service: service, account: account, label: label, trustedPaths: [KeychainNames.securityTool])
    }
}

/// An in-memory store, for tests and for platforms without a Keychain.
public final class InMemorySecretStore: SecretStore, @unchecked Sendable {
    public struct Item: Equatable {
        public var secret: String
        public var label: String
        public var trustedPaths: [String]
    }

    private let lock = NSLock()
    private var items: [String: Item] = [:]

    public init() {}

    private func key(_ service: String, _ account: String) -> String { service + "\u{0}" + account }

    public func item(service: String, account: String) -> Item? {
        lock.lock(); defer { lock.unlock() }
        return items[key(service, account)]
    }

    public func read(service: String, account: String) throws -> String? {
        item(service: service, account: account)?.secret
    }

    public func exists(service: String, account: String) throws -> Bool {
        item(service: service, account: account) != nil
    }

    public func write(_ secret: String, service: String, account: String, label: String, trustedPaths: [String]) throws {
        lock.lock(); defer { lock.unlock() }
        items[key(service, account)] = Item(secret: secret, label: label, trustedPaths: trustedPaths)
    }

    public func delete(service: String, account: String) throws {
        lock.lock(); defer { lock.unlock() }
        items[key(service, account)] = nil
    }
}

#if canImport(Security) && os(macOS)
/// The login keychain, through SecItem.
///
/// Items are created in the file-based login keychain (no `kSecUseDataProtectionKeychain`),
/// which is the keychain `/usr/bin/security` reads and the only one whose items carry an
/// access list naming other applications.
public struct KeychainStore: SecretStore {
    public init() {}

    private func query(_ service: String, _ account: String) -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }

    public func read(service: String, account: String) throws -> String? {
        var q = query(service, account)
        q[kSecReturnData as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &out)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = out as? Data else {
            throw SecretStoreError.status("reading \(service)/\(account) from the Keychain", status)
        }
        return String(decoding: data, as: UTF8.self)
    }

    public func exists(service: String, account: String) throws -> Bool {
        var q = query(service, account)
        q[kSecReturnAttributes as String] = true
        q[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let status = SecItemCopyMatching(q as CFDictionary, &out)
        if status == errSecItemNotFound { return false }
        guard status == errSecSuccess else {
            throw SecretStoreError.status("looking up \(service)/\(account) in the Keychain", status)
        }
        return true
    }

    public func write(_ secret: String, service: String, account: String, label: String, trustedPaths: [String]) throws {
        // Replaced rather than updated, so the item's access list is always the one asked
        // for here, whoever created the previous one.
        try delete(service: service, account: account)
        var attributes = query(service, account)
        attributes[kSecValueData as String] = Data(secret.utf8)
        attributes[kSecAttrLabel as String] = label
        if !trustedPaths.isEmpty {
            do {
                attributes[kSecAttrAccess as String] = try TrustedAccess.make(label: label, includeSelf: true, paths: trustedPaths)
            } catch {
                throw SecretStoreError.access(String(describing: error))
            }
        }
        let status = SecItemAdd(attributes as CFDictionary, nil)
        guard status == errSecSuccess else {
            throw SecretStoreError.status("saving \(service)/\(account) to the Keychain", status)
        }
    }

    public func delete(service: String, account: String) throws {
        let status = SecItemDelete(query(service, account) as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw SecretStoreError.status("removing \(service)/\(account) from the Keychain", status)
        }
    }
}
#endif

/// The store this platform has: the Keychain on macOS, memory elsewhere.
public func defaultSecretStore() -> SecretStore {
    #if canImport(Security) && os(macOS)
    return KeychainStore()
    #else
    return InMemorySecretStore()
    #endif
}
