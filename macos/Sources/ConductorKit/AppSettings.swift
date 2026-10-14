import Foundation

/// Somewhere to keep small settings: UserDefaults in the app, a dictionary in tests.
public protocol SettingsStore: AnyObject {
    func object(forKey key: String) -> Any?
    func set(_ value: Any?, forKey key: String)
}

extension UserDefaults: SettingsStore {}

public final class MemorySettingsStore: SettingsStore {
    private var values: [String: Any] = [:]
    public init() {}
    public func object(forKey key: String) -> Any? { values[key] }
    public func set(_ value: Any?, forKey key: String) { values[key] = value }
}

/// The app's own settings (Settings → General). Nothing secret is ever kept here: tokens and
/// keys live in the Keychain.
public struct AppSettings: Equatable, Sendable {
    public enum Key {
        public static let daemonPort = "daemonPort"
        public static let startAtLogin = "startAtLogin"
        public static let publicURL = "publicURL"
        public static let attachEndpoint = "attachEndpoint"
        public static let usesExternalDatabase = "usesExternalDatabase"
        public static let project = "project"
        public static let repositories = "repositories"
        public static let onboardingComplete = "onboardingComplete"
        public static let toolsSkipped = "toolsSkipped"
        public static let githubSkipped = "githubSkipped"
    }

    public static let defaultPort = 8080

    /// The loopback port conductord listens on.
    public var daemonPort: Int = AppSettings.defaultPort
    /// Load the agents at login (RunAtLoad), and open the app at login.
    public var startAtLogin = true
    /// conductord's `--public-url`: the address teammates reach it at, used in invite links.
    public var publicURL = ""
    /// A control plane this app does not run; when set, the app supervises nothing and only
    /// signs in to it and shows it.
    public var attachEndpoint = ""
    /// A Postgres the person already runs; when set, the bundled one is not started.
    /// A DSN can hold a password, so it is not kept here: the app keeps it in the Keychain.
    public var usesExternalDatabase = false
    /// The project the menu bar and the event stream follow (a slug).
    public var project = ""
    /// Repository folders onboarding initialised, most recent first.
    public var repositories: [String] = []
    public var onboardingComplete = false
    public var toolsSkipped = false
    public var githubSkipped = false

    public init() {}

    public static func load(from store: SettingsStore) -> AppSettings {
        var s = AppSettings()
        if let p = store.object(forKey: Key.daemonPort) as? Int, p > 0, p < 65536 { s.daemonPort = p }
        if let b = store.object(forKey: Key.startAtLogin) as? Bool { s.startAtLogin = b }
        s.publicURL = store.object(forKey: Key.publicURL) as? String ?? ""
        s.attachEndpoint = store.object(forKey: Key.attachEndpoint) as? String ?? ""
        s.usesExternalDatabase = store.object(forKey: Key.usesExternalDatabase) as? Bool ?? false
        s.project = store.object(forKey: Key.project) as? String ?? ""
        s.repositories = store.object(forKey: Key.repositories) as? [String] ?? []
        s.onboardingComplete = store.object(forKey: Key.onboardingComplete) as? Bool ?? false
        s.toolsSkipped = store.object(forKey: Key.toolsSkipped) as? Bool ?? false
        s.githubSkipped = store.object(forKey: Key.githubSkipped) as? Bool ?? false
        return s
    }

    public func save(to store: SettingsStore) {
        store.set(daemonPort, forKey: Key.daemonPort)
        store.set(startAtLogin, forKey: Key.startAtLogin)
        store.set(publicURL, forKey: Key.publicURL)
        store.set(attachEndpoint, forKey: Key.attachEndpoint)
        store.set(usesExternalDatabase, forKey: Key.usesExternalDatabase)
        store.set(project, forKey: Key.project)
        store.set(repositories, forKey: Key.repositories)
        store.set(onboardingComplete, forKey: Key.onboardingComplete)
        store.set(toolsSkipped, forKey: Key.toolsSkipped)
        store.set(githubSkipped, forKey: Key.githubSkipped)
    }

    /// Whether this app runs the control plane, or only shows one someone else runs.
    public var isAttached: Bool { !attachEndpoint.trimmingCharacters(in: .whitespaces).isEmpty }

    /// The control plane's URL: the attached one, else the daemon this app runs on loopback.
    /// 127.0.0.1 rather than localhost, so the address never resolves to ::1 where the
    /// daemon is not listening.
    public var endpoint: String {
        isAttached ? attachEndpoint.trimmingCharacters(in: .whitespaces) : "http://127.0.0.1:\(daemonPort)"
    }

    public mutating func remember(repository: String) {
        repositories.removeAll { $0 == repository }
        repositories.insert(repository, at: 0)
        if repositories.count > 10 { repositories.removeLast(repositories.count - 10) }
    }
}
