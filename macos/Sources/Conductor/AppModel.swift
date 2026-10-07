import AppKit
import ConductorKit
import Foundation
import SwiftUI

/// Everything the windows, the menu bar and the sheets show, and every action they take.
/// Logic that can be tested lives in ConductorKit; this type wires it to the screen.
@MainActor
final class AppModel: ObservableObject {
    static let shared = AppModel()

    enum Sheet: String, Identifiable {
        case invite, tools, github, join
        var id: String { rawValue }
    }

    enum SignIn: Equatable {
        case unknown
        case signedIn(handle: String)
        case needsOwner
        case tokenRequired(String)
        case failed(String)

        var handle: String? {
            if case .signedIn(let h) = self { return h }
            return nil
        }
    }

    // MARK: published state

    @Published var settings: AppSettings
    @Published var phase: SupervisorPhase = .idle
    @Published var daemonUp = false
    @Published var signIn: SignIn = .unknown
    @Published var localStatus: LocalStatus?
    @Published var projects: [WhoAmI.Project] = []
    @Published var status: StatusSummary?
    @Published var offers: [Assignment] = []
    @Published var pullRequests: [PullRequestChecks.Row] = []
    @Published var dot: StatusDot = .down
    /// Archiving to the bucket is failing, from `conductor db status --json --local`.
    @Published var databaseProblem: String?
    @Published var storage: StorageShow?
    /// Base backups found in the bucket on a first run: the onboarding offers a restore.
    @Published var restoreOffer: BaseBackupList?
    @Published var doctor: DoctorReport?
    @Published var github: GitHubStatus?
    @Published var checkpointGroups: [CheckpointGroup] = []
    @Published var checkpointError: String?
    /// What is running right now, in words, for a spinner.
    @Published var busy: String?
    /// The last thing that went wrong, for the window that caused it.
    @Published var problem: String?
    @Published var sheet: Sheet?
    @Published var pendingJoin: InviteLink.Join?
    /// A window to open; MenuBarLabel, which is always alive, opens it.
    @Published var windowRequest: String?
    @Published var dashboardReloads = 0

    // MARK: plumbing

    let paths = AppPaths.current()
    let secrets: SecretStore = defaultSecretStore()
    let binaries: ConductorBinaries?
    private(set) var supervisor: Supervisor?
    private(set) var token: String?
    private var stream: EventStream?
    private var limits = UsageLimitWatcher()
    private var prChecks = PullRequestChecks()
    private var refreshPending = false
    private var timer: Timer?
    private var ticks = 0
    private let defaults = UserDefaults.standard

    init() {
        settings = AppSettings.load(from: UserDefaults.standard)
        binaries = ConductorBinaries.locate(resources: Bundle.main.resourceURL,
                                            environment: ProcessInfo.processInfo.environment)
    }

    var endpointURL: URL { URL(string: settings.endpoint) ?? URL(string: "http://127.0.0.1:8080")! }
    var api: APIClient { APIClient(endpoint: endpointURL, token: token) }
    var signedIn: Bool { signIn.handle != nil }
    var project: String? { settings.project.isEmpty ? projects.first?.slug : settings.project }
    var environment: [String: String] { ProcessInfo.processInfo.environment }

    /// The dashboard's address: the same origin, so it signs itself in locally.
    var dashboardURL: URL {
        var s = settings.endpoint.hasSuffix("/") ? settings.endpoint : settings.endpoint + "/"
        if let project, !project.isEmpty { s += "#project=" + InviteLink.queryEscape(project) }
        return URL(string: s) ?? endpointURL
    }

    func saveSettings() { settings.save(to: defaults) }

    // MARK: - launch

    func launch() async {
        token = try? secrets.read(service: KeychainNames.tokenService, account: settings.endpoint)
        if settings.isAttached {
            phase = .running
            await connect()
            startTimer()
            return
        }
        guard let binaries else {
            phase = .failed("The conductor commands are missing from this app (Contents/Resources/bin). Install Conductor again.")
            return
        }
        supervisor = makeSupervisor(binaries)
        storage = await loadStorage()
        // A first run on a Mac whose bucket already holds this person's database: offer to
        // restore it rather than start empty. The onboarding asks; nothing starts until then.
        if let pg = supervisor?.config.postgres, !pg.isInitialized(), storage?.databaseToBucket == true {
            let r = try? await runConductor(ConductorCommands.dbBackups)
            let backups = r.flatMap { BaseBackupList.decode($0.stdout) }
            if RestoreOffer.shouldOffer(clusterExists: false, storage: storage, backups: backups) {
                restoreOffer = backups
                return
            }
        }
        await startServices(restore: false)
        startTimer()
    }

    private func makeSupervisor(_ binaries: ConductorBinaries) -> Supervisor {
        var external: String?
        if settings.usesExternalDatabase {
            external = try? secrets.read(service: KeychainNames.databaseService, account: KeychainNames.databaseAccount)
        }
        let config = SupervisorConfig(paths: paths, binaries: binaries, settings: settings, uid: UInt32(getuid()),
                                      baseEnvironment: environment, externalDSN: external)
        return Supervisor(config: config)
    }

    /// Starts (or restarts) the database and the daemon. `restore` is the answer to
    /// "Restore from bucket".
    func startServices(restore: Bool) async {
        guard let binaries else { return }
        restoreOffer = nil
        if supervisor == nil { supervisor = makeSupervisor(binaries) }
        if settings.daemonPort == AppSettings.defaultPort, !settings.onboardingComplete,
           !PortProbe.isFree(settings.daemonPort), !(await api.isHealthy()),
           let free = PortProbe.firstFree(from: AppSettings.defaultPort + 1) {
            // First run, and something that is not Conductor holds 8080.
            settings.daemonPort = free
            saveSettings()
            supervisor = makeSupervisor(binaries)
        }
        problem = nil
        do {
            try await supervisor?.ensureRunning(storage: storage, restoreFromBucket: restore) { [weak self] p in
                Task { @MainActor in self?.phase = p }
            }
            phase = .running
        } catch {
            phase = .failed(String(describing: error))
            problem = String(describing: error)
        }
        await connect()
    }

    /// Applies changed Settings (port, start at login, storage): rewrites the agents and
    /// restarts what changed.
    func restartServices() async {
        stopStreaming()
        token = try? secrets.read(service: KeychainNames.tokenService, account: settings.endpoint)
        if settings.isAttached {
            supervisor = nil
            await connect()
            return
        }
        guard let binaries else { return }
        supervisor = makeSupervisor(binaries)
        storage = await loadStorage()
        await startServices(restore: false)
    }

    /// Saved storage settings reach the running system: archiving written (or taken out)
    /// before Postgres restarts, which happens only when what it reads changed, and the
    /// base-backup agent installed or removed.
    func applyStorage(_ show: StorageShow) async {
        storage = show
        guard let supervisor, !settings.isAttached, supervisor.config.postgres != nil else { return }
        do {
            try await supervisor.ensureRunning(storage: show) { [weak self] p in
                Task { @MainActor in self?.phase = p }
            }
            phase = .running
        } catch {
            problem = String(describing: error)
        }
        await connect()
    }

    func stopServices() async {
        stopStreaming()
        await supervisor?.stop()
        daemonUp = false
        dot = .down
        phase = .stopped
    }

    // MARK: - signing in

    func connect() async {
        daemonUp = await api.isHealthy()
        guard daemonUp else {
            dot = .down
            return
        }
        await signInIfNeeded()
        await refresh()
        startStreaming()
    }

    /// A saved token that still works, else local sign-in: `GET /v1/local/status`, then
    /// `POST /v1/local/session {"client":"mac-app"}`, the token kept in the Keychain.
    func signInIfNeeded() async {
        if token != nil, let who = try? await api.whoami() {
            accept(who)
            return
        }
        token = nil
        do {
            let s = try await api.localStatus()
            localStatus = s
            switch OnboardingFlow.signIn(s) {
            case .automatic:
                let session = try await api.localSession()
                token = session.token
                try? secrets.write(session.token, service: KeychainNames.tokenService, account: settings.endpoint,
                                   label: "Conductor sign-in (\(settings.endpoint))", trustedPaths: [])
                accept(try await api.whoami())
                await ensureCLILogin()
            case .needsOwner:
                signIn = .needsOwner
            case .tokenRequired(let why), .unavailable(let why):
                signIn = .tokenRequired(why)
            }
        } catch {
            signIn = .failed(String(describing: error))
        }
    }

    private func accept(_ who: WhoAmI) {
        signIn = .signedIn(handle: who.principal.handle)
        projects = who.projects
        if settings.project.isEmpty || !who.projects.contains(where: { $0.slug == settings.project }) {
            settings.project = who.projects.first?.slug ?? ""
            saveSettings()
        }
    }

    /// Signs in with a token from enhanced mode or a teammate.
    func signIn(token: String) async {
        self.token = token.trimmingCharacters(in: .whitespacesAndNewlines)
        do {
            accept(try await api.whoami())
            try? secrets.write(self.token ?? "", service: KeychainNames.tokenService, account: settings.endpoint,
                               label: "Conductor sign-in (\(settings.endpoint))", trustedPaths: [])
            await refresh()
            startStreaming()
        } catch {
            self.token = nil
            signIn = .failed(String(describing: error))
        }
    }

    /// The CLI, and through it the coding tools' MCP servers, need their own login: `conductor
    /// login` against this loopback endpoint signs in locally too.
    private func ensureCLILogin() async {
        guard !settings.isAttached else { return }
        _ = try? await runConductor(ConductorCommands.login(endpoint: settings.endpoint))
    }

    // MARK: - status and offers

    func refresh() async {
        guard let project, signedIn else {
            dot = StatusDot.compute(daemonUp: daemonUp, status: nil, offers: [], databaseFailing: databaseProblem != nil)
            return
        }
        do {
            let s = try await api.status(project: project)
            status = s
            var found: [Assignment] = []
            if let me = signIn.handle {
                for session in s.sessions(of: me) {
                    if let list = try? await api.assignments(session: session.sessionID) {
                        found += list.filter(\.isOffered)
                    }
                }
            }
            offers = found
            pullRequests = prChecks.rows(s)
        } catch let e as APIError where e.isUnauthenticated {
            token = nil
            await signInIfNeeded()
        } catch {
            daemonUp = await api.isHealthy()
        }
        dot = StatusDot.compute(daemonUp: daemonUp, status: status, offers: offers, databaseFailing: databaseProblem != nil)
    }

    /// Collapses a burst of events into one refresh.
    func scheduleRefresh() {
        guard !refreshPending else { return }
        refreshPending = true
        Task {
            try? await Task.sleep(nanoseconds: 400_000_000)
            refreshPending = false
            await refresh()
        }
    }

    func respond(to offer: Assignment, accept: Bool) async {
        do {
            _ = try await api.respond(assignment: offer.id, accept: accept)
        } catch {
            problem = String(describing: error)
        }
        await refresh()
    }

    func acknowledge(_ conflict: ConflictView) async {
        do {
            try await api.acknowledge(conflict: conflict.id)
        } catch {
            problem = String(describing: error)
        }
        await refresh()
    }

    // MARK: - the event stream

    func startStreaming() {
        stopStreaming()
        guard let project, let token else { return }
        let s = EventStream(url: api.eventStreamURL(project: project), token: token, onEvent: { event in
            Task { @MainActor in AppModel.shared.handle(event) }
        }, onState: { state in
            Task { @MainActor in AppModel.shared.streamState(state) }
        })
        stream = s
        s.start()
    }

    func stopStreaming() {
        stream?.stop()
        stream = nil
    }

    private func streamState(_ state: EventStream.State) {
        switch state {
        case .unauthorized:
            token = nil
            Task { await signInIfNeeded(); startStreaming() }
        case .waiting:
            Task {
                daemonUp = await api.isHealthy()
                dot = StatusDot.compute(daemonUp: daemonUp, status: status, offers: offers, databaseFailing: databaseProblem != nil)
            }
        default:
            break
        }
    }

    private func handle(_ sse: SSEEvent) {
        guard let event = DomainEvent.decode(sse) else { return }
        if prChecks.record(event) { pullRequests = prChecks.rows(status) }
        switch EventReaction.classify(event) {
        case .refresh:
            scheduleRefresh()
        case .usageLimit(_, let detail):
            Notifier.shared.usageLimit(title: "Usage limit reached", body: detail + " Continue the session under another login or in another tool.")
        case .ignore:
            break
        }
    }

    // MARK: - polling

    private func startTimer() {
        timer?.invalidate()
        timer = Timer.scheduledTimer(withTimeInterval: 30, repeats: true) { _ in
            Task { @MainActor in await AppModel.shared.tick() }
        }
        Task { await loadCheckpoints() }
    }

    /// Every 30 seconds: the daemon's health, the status, and new checkpoints taken because a
    /// login ran out (the first-version usage-limit signal: `conductor wrap` checkpoints a
    /// session with reason "quota" when its login reaches the limit).
    private func tick() async {
        ticks += 1
        let wasUp = daemonUp
        daemonUp = await api.isHealthy()
        if daemonUp && (!wasUp || stream == nil) {
            await connect()
        } else {
            await refresh()
        }
        if ticks % 2 == 0 { await loadCheckpoints() }
        await checkDatabase()
    }

    /// When the database goes to the bucket: `conductor db status --json --local`, which
    /// reads only what this Mac recorded and needs no network, so it is cheap to poll.
    private func checkDatabase() async {
        guard storage?.databaseToBucket == true, !settings.isAttached,
              let r = try? await runConductor(ConductorCommands.dbStatusLocal),
              let status = DatabaseStatus.decode(r.stdout) else {
            databaseProblem = nil
            return
        }
        databaseProblem = status.problem
        dot = StatusDot.compute(daemonUp: daemonUp, status: self.status, offers: offers, databaseFailing: databaseProblem != nil)
    }

    // MARK: - checkpoints

    func loadCheckpoints() async {
        do {
            let r = try await runConductor(ConductorCommands.checkpointList)
            guard r.succeeded else {
                checkpointError = r.failureMessage("conductor checkpoint list")
                return
            }
            let list = try CheckpointManifest.decodeList(r.stdout)
            checkpointGroups = CheckpointGroup.group(list)
            checkpointError = nil
            for m in limits.newLimits(in: list) {
                Notifier.shared.usageLimit(
                    title: "\(Harness.normalize(m.harness)?.title ?? m.harness) hit its usage limit",
                    body: "\(m.displayTitle) was checkpointed. Continue it under another login or in another tool.")
            }
        } catch {
            checkpointError = String(describing: error)
        }
    }

    func accounts(for harness: String) -> [String] {
        Accounts.list(harness: Harness.normalize(harness)?.rawValue ?? harness, home: paths.home,
                      conductorState: paths.conductorState)
    }

    /// Continues a checkpoint in a Terminal window, the way `conductor resume` opens one.
    func resume(_ m: CheckpointManifest, _ target: ResumeTarget) async {
        guard let binaries else { return }
        let argv = [binaries.conductor.path] + target.arguments(checkpoint: m.id)
        let fm = FileManager.default
        let cwd = [m.cwd, m.repo?.root ?? ""].first { !$0.isEmpty && fm.fileExists(atPath: $0) } ?? paths.home.path
        let launch = TerminalLauncher.launchArguments(cwd: cwd, argv: argv, environment: environment)
        guard let exe = launch.first else { return }
        do {
            let r = try await ProcessRunner().run(CommandSpec(URL(fileURLWithPath: exe.hasPrefix("/") ? exe : "/usr/bin/env"),
                                                              exe.hasPrefix("/") ? Array(launch.dropFirst()) : launch,
                                                              environment: commandEnvironment))
            if !r.succeeded { problem = r.failureMessage("Opening Terminal") }
        } catch {
            problem = String(describing: error)
        }
    }

    // MARK: - commands

    var commandEnvironment: [String: String] {
        binaries?.environment(base: environment, home: paths.home) ?? environment
    }

    /// The bundled `conductor`, with the app's environment.
    func runConductor(_ args: [String], stdin: Data? = nil, cwd: URL? = nil) async throws -> CommandResult {
        if let supervisor { return try await supervisor.conductor(args, stdin: stdin, cwd: cwd) }
        guard let binaries else { throw CommandError.launch("conductor", "the app carries no conductor command") }
        return try await ProcessRunner().run(CommandSpec(binaries.conductor, args, environment: commandEnvironment,
                                                         currentDirectory: cwd, stdin: stdin))
    }

    /// Runs a command for a button: a spinner while it runs, the error if it fails.
    @discardableResult
    func perform(_ what: String, _ args: [String], cwd: URL? = nil) async -> CommandResult? {
        busy = what
        defer { busy = nil }
        do {
            let r = try await runConductor(args, cwd: cwd)
            if !r.succeeded { problem = r.failureMessage("conductor \(args.first ?? "")") }
            return r
        } catch {
            problem = String(describing: error)
            return nil
        }
    }

    func loadStorage() async -> StorageShow? {
        guard let r = try? await runConductor(ConductorCommands.storageShow), r.succeeded else { return nil }
        return try? StorageShow.decode(r.stdout)
    }

    func pauseAll() async {
        await perform("Pausing every agent…", ConductorCommands.pause)
    }

    func resumeAll() async {
        await perform("Waking every agent…", ConductorCommands.resume)
    }

    // MARK: - onboarding

    var onboardingFacts: OnboardingFacts {
        var f = OnboardingFacts()
        f.servicesRunning = daemonUp
        f.signedIn = signedIn
        f.localStatus = localStatus
        f.hasProject = !projects.isEmpty
        f.toolsConnected = doctor?.anyToolConnected ?? false
        f.toolsSkipped = settings.toolsSkipped
        f.githubConfigured = (github?.stage ?? .noApp) != .noApp
        f.githubSkipped = settings.githubSkipped
        return f
    }

    var onboardingStep: OnboardingStep { OnboardingFlow.next(onboardingFacts) }

    /// Pick a repository: `conductor init` there, then `conductord bootstrap`, which makes
    /// its project and, on a fresh database, makes the person this machine's owner.
    func chooseRepository(_ folder: URL) async {
        busy = "Setting up \(folder.lastPathComponent)…"
        defer { busy = nil }
        problem = nil
        do {
            let r = try await runConductor(ConductorCommands.initRepository(folder))
            guard r.succeeded else { problem = r.failureMessage("conductor init"); return }
            if let supervisor {
                _ = try await supervisor.bootstrapOwner(repository: folder)
            }
            settings.remember(repository: folder.path)
            settings.project = ""
            saveSettings()
            await signInIfNeeded()
            await refresh()
            startStreaming()
        } catch {
            problem = String(describing: error)
        }
    }

    func loadDoctor() async {
        guard let r = try? await runConductor(ConductorCommands.doctor) else { return }
        doctor = try? DoctorReport.decode(r.stdout)
    }

    func connectTools(_ tool: String? = nil) async {
        await perform(tool.map { "Connecting \($0)…" } ?? "Connecting your tools…",
                      tool.map(ConductorCommands.integrate) ?? ConductorCommands.integrateAll)
        await loadDoctor()
    }

    func finishOnboarding() {
        settings.onboardingComplete = true
        saveSettings()
    }

    // MARK: - GitHub

    func loadGitHub() async {
        guard let r = try? await runConductor(ConductorCommands.githubStatus), r.succeeded else { return }
        github = try? GitHubStatus.decode(r.stdout)
    }

    /// `conductor github setup`: a one-time page on this control plane that creates the app
    /// on GitHub in two clicks; opened in the person's browser, where they are signed in.
    func createGitHubApp(org: String) async {
        guard let r = await perform("Preparing the GitHub App…", ConductorCommands.githubSetup(org: org)),
              r.succeeded, let setup = try? JSONDecoder().decode(GitHubSetup.self, from: r.stdout),
              let url = URL(string: setup.setupURL) else { return }
        NSWorkspace.shared.open(url)
    }

    func installGitHubApp() {
        guard let s = github?.installURL, let url = URL(string: s) else { return }
        NSWorkspace.shared.open(url)
    }

    func linkGitHub(repository folder: URL) async {
        await perform("Linking \(folder.lastPathComponent)…", ConductorCommands.githubLink(project: project), cwd: folder)
        await loadGitHub()
    }

    // MARK: - links

    /// `conductor://join#endpoint=…&project=…&token=…`: asks before joining.
    func open(_ url: URL) {
        switch InviteLink.parse(url.absoluteString) {
        case .success(let join):
            pendingJoin = join
            sheet = .join
            requestWindow(WindowID.dashboard)
        case .failure(let why):
            problem = why.description
        }
    }

    /// Joins with `conductor join`, then connects every coding tool on this Mac.
    func join(_ j: InviteLink.Join) async {
        guard let r = await perform("Joining \(j.endpoint)…", ConductorCommands.join(link: j.webLink)), r.succeeded else { return }
        await connectTools()
        pendingJoin = nil
        sheet = nil
        if let result = try? JSONDecoder().decode(JoinResult.self, from: r.stdout) {
            Notifier.shared.post(title: "Joined \(result.project ?? j.endpoint)",
                                 body: "Signed in as \(result.handle ?? "you"). Your coding tools now have Conductor's MCP tools.")
        }
    }

    // MARK: - windows

    func present(_ s: Sheet) {
        sheet = s
        requestWindow(WindowID.dashboard)
    }

    func requestWindow(_ id: String) {
        windowRequest = id
    }

    func reloadDashboard() { dashboardReloads += 1 }
}
