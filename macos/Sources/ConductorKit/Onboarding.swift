import Foundation

/// The first-run flow (docs/MACOS_APP.md, Screens): start the database and daemon, sign in,
/// pick a repository, connect tools, and optionally GitHub.
public enum OnboardingStep: Int, CaseIterable, Comparable, Identifiable, Sendable {
    case services
    case signIn
    case repository
    case tools
    case github
    case done

    public var id: Int { rawValue }

    public static func < (a: OnboardingStep, b: OnboardingStep) -> Bool { a.rawValue < b.rawValue }

    public var title: String {
        switch self {
        case .services: return "Start Conductor"
        case .signIn: return "Sign in"
        case .repository: return "Pick a repository"
        case .tools: return "Connect your coding tools"
        case .github: return "GitHub (optional)"
        case .done: return "Done"
        }
    }
}

/// What the app knows when it decides which step to show.
public struct OnboardingFacts: Equatable, Sendable {
    public var servicesRunning = false
    public var signedIn = false
    public var localStatus: LocalStatus?
    public var hasProject = false
    public var toolsConnected = false
    public var toolsSkipped = false
    public var githubConfigured = false
    public var githubSkipped = false

    public init() {}

    /// Nobody owns this machine yet. Signing in waits for the repository step, whose
    /// `conductord bootstrap` makes the person picking it the owner.
    public var waitingForOwner: Bool { localStatus?.needsOwner ?? false }
}

public enum OnboardingFlow {
    /// The first step not done yet.
    ///
    /// Sign-in comes second, as the plan has it, except on a fresh database: there it can only
    /// happen after the repository step has bootstrapped an owner, so the flow goes to the
    /// repository and comes back to sign in.
    public static func next(_ f: OnboardingFacts) -> OnboardingStep {
        if !f.servicesRunning { return .services }
        if !f.signedIn && !f.waitingForOwner { return .signIn }
        if !f.hasProject { return .repository }
        if !f.signedIn { return .signIn }
        if !f.toolsConnected && !f.toolsSkipped { return .tools }
        if !f.githubConfigured && !f.githubSkipped { return .github }
        return .done
    }

    /// How sign-in goes on this server, from `GET /v1/local/status`.
    public enum SignIn: Equatable, Sendable {
        /// Local sign-in will work: call `POST /v1/local/session`.
        case automatic
        /// Nobody owns the machine yet; the repository step makes the owner.
        case needsOwner
        /// Enhanced security mode: a token, or a join link, is needed.
        case tokenRequired(String)
        /// Local mode, but this request cannot sign in locally, and why.
        case unavailable(String)
    }

    public static func signIn(_ s: LocalStatus) -> SignIn {
        if s.localLoginAvailable { return .automatic }
        if s.needsOwner { return .needsOwner }
        if s.securityMode != "local" {
            return .tokenRequired(s.reason ?? "This Conductor is in enhanced security mode: sign in with a token or a join link.")
        }
        return .unavailable(s.reason ?? "Local sign-in is not available.")
    }
}
