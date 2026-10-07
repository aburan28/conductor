import AppKit
import ConductorKit
import SwiftUI

/// First run: start the database and daemon, sign in, pick a repository, connect the coding
/// tools, and optionally GitHub (docs/MACOS_APP.md, Screens).
struct OnboardingView: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        let current = model.onboardingStep
        HStack(spacing: 0) {
            VStack(alignment: .leading, spacing: 14) {
                Text("Welcome to Conductor").font(.title2.bold())
                ForEach(OnboardingStep.allCases.filter { $0 != .done }) { step in
                    HStack(spacing: 8) {
                        Image(systemName: step < current ? "checkmark.circle.fill" : (step == current ? "circle.inset.filled" : "circle"))
                            .foregroundStyle(step < current ? Color.green : (step == current ? Color.accentColor : Color.secondary))
                        Text(step.title).fontWeight(step == current ? .semibold : .regular)
                    }
                }
                Spacer()
                if current != .done {
                    Button("Skip setup") { model.finishOnboarding() }
                        .buttonStyle(.link)
                        .help("Go straight to the dashboard; Settings and the Conductor menu have every step")
                }
            }
            .padding(24)
            .frame(width: 240, alignment: .leading)
            .frame(maxHeight: .infinity, alignment: .top)
            .background(Color(nsColor: .controlBackgroundColor))

            Divider()

            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    switch current {
                    case .services: ServicesStep()
                    case .signIn: SignInStep()
                    case .repository: RepositoryStep()
                    case .tools: ToolsStep()
                    case .github: GitHubStep()
                    case .done: DoneStep()
                    }
                    if let problem = model.problem {
                        Text(problem)
                            .foregroundStyle(.red).font(.callout).textSelection(.enabled)
                            .fixedSize(horizontal: false, vertical: true)
                    }
                    if let busy = model.busy {
                        HStack { ProgressView().controlSize(.small); Text(busy).font(.callout) }
                    }
                }
                .padding(32)
                .frame(maxWidth: 620, alignment: .leading)
            }
            .frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
        }
    }
}

private struct StepHeader: View {
    let title: String
    let detail: String

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            Text(title).font(.title.bold())
            Text(detail).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }
}

private struct ServicesStep: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        StepHeader(title: "Start Conductor",
                   detail: "Conductor runs its control plane and a private database on this Mac, and starts them when you log in. No Docker, nothing listening beyond this Mac.")
        if let offer = model.restoreOffer {
            GroupBox {
                VStack(alignment: .leading, spacing: 10) {
                    Text("Your storage bucket holds a backup of a Conductor database.").font(.headline)
                    if let latest = offer.latest {
                        Text("\(offer.count) base backup\(offer.count == 1 ? "" : "s"); the newest is \(latest.takenAt ?? latest.id). Restoring brings back every project, task and login up to the last archived change.")
                            .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }
                    HStack {
                        Button("Restore from Bucket") { Task { await model.startServices(restore: true) } }
                            .keyboardShortcut(.defaultAction)
                        Button("Start Empty") { Task { await model.startServices(restore: false) } }
                    }
                }
                .padding(6)
            }
        } else {
            HStack(spacing: 10) {
                switch model.phase {
                case .running:
                    Image(systemName: "checkmark.circle.fill").foregroundStyle(.green)
                case .failed:
                    Image(systemName: "exclamationmark.triangle.fill").foregroundStyle(.orange)
                default:
                    ProgressView().controlSize(.small)
                }
                Text(model.phase.label).fixedSize(horizontal: false, vertical: true)
            }
            if model.phase.isFailed {
                HStack {
                    Button("Try Again") { Task { await model.restartServices() } }
                    Button("Settings…") { openSettings() }
                }
            }
        }
    }
}

private struct SignInStep: View {
    @EnvironmentObject var model: AppModel
    @State private var token = ""

    var body: some View {
        StepHeader(title: "Sign in",
                   detail: "On this Mac you are signed in without a token: the control plane trusts requests from this machine's owner (local security mode).")
        switch model.signIn {
        case .tokenRequired(let why), .failed(let why):
            Text(why).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            HStack {
                SecureField("Token (cdt_…)", text: $token).frame(maxWidth: 360)
                Button("Sign In") { Task { await model.signIn(token: token) } }
                    .disabled(token.isEmpty)
            }
            Text("A teammate's invite link works too: open it, and this app joins with it.")
                .font(.callout).foregroundStyle(.secondary)
            Button("Try Local Sign-in Again") { Task { await model.signInIfNeeded() } }
        default:
            HStack {
                ProgressView().controlSize(.small)
                Text("Signing in…")
            }
            .task { await model.signInIfNeeded() }
        }
    }
}

@MainActor
private struct RepositoryStep: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        StepHeader(title: "Pick a repository",
                   detail: "Choose a checkout your agents work in. Conductor writes its policy files there (.conductor/, and a short block in CLAUDE.md and AGENTS.md) and creates a project for it.")
        Button("Choose Folder…") { choose() }
            .keyboardShortcut(.defaultAction)
            .disabled(model.busy != nil)
        if model.signIn == .needsOwner {
            Text("This is a new Conductor, so the person who picks the first repository owns this Mac's control plane and is signed in automatically from now on.")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
        }
    }

    private func choose() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.prompt = "Use This Repository"
        panel.message = "Choose the folder of a Git checkout."
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task { await model.chooseRepository(url) }
    }
}

private struct ToolsStep: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        StepHeader(title: "Connect your coding tools",
                   detail: "Each tool gets Conductor's MCP server and, where it has them, hooks that check for conflicts before an edit (`conductor integrate all --global`).")
        ToolsList()
        HStack {
            Button("Connect All") { Task { await model.connectTools() } }
                .keyboardShortcut(.defaultAction)
                .disabled(model.busy != nil)
            Button("Skip") {
                model.settings.toolsSkipped = true
                model.saveSettings()
            }
        }
        .task { await model.loadDoctor() }
    }
}

private struct GitHubStep: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        StepHeader(title: "GitHub (optional)",
                   detail: "A GitHub App puts a Conductor check on every pull request that collides with someone's open work.")
        GitHubPanel()
        Button("Skip") {
            model.settings.githubSkipped = true
            model.saveSettings()
        }
    }
}

private struct DoneStep: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        StepHeader(title: "You are set",
                   detail: "Conductor is running and your tools are connected. The menu bar dot shows who is live and what is contested; Invite someone by text from the toolbar.")
        Button("Open the Dashboard") { model.finishOnboarding() }
            .keyboardShortcut(.defaultAction)
    }
}

/// The per-tool list from `conductor doctor --json`, shared by onboarding and the sheet.
struct ToolsList: View {
    @EnvironmentObject var model: AppModel

    var body: some View {
        if let doctor = model.doctor {
            VStack(alignment: .leading, spacing: 8) {
                ForEach(doctor.integrations) { tool in
                    HStack {
                        Image(systemName: icon(tool.state)).foregroundStyle(color(tool.state))
                        Text(tool.title).frame(width: 140, alignment: .leading)
                        Text(tool.state.label).foregroundStyle(.secondary).font(.callout)
                        Spacer()
                        if tool.detected && tool.state != .connected {
                            Button("Connect") { Task { await model.connectTools(tool.tool) } }
                                .disabled(model.busy != nil)
                        }
                    }
                }
            }
        } else {
            HStack { ProgressView().controlSize(.small); Text("Looking for coding tools…") }
        }
    }

    private func icon(_ s: DoctorReport.Integration.State) -> String {
        switch s {
        case .connected: return "checkmark.circle.fill"
        case .connectedWithoutHooks: return "checkmark.circle"
        case .notConnected: return "circle"
        case .notInstalled: return "minus.circle"
        }
    }

    private func color(_ s: DoctorReport.Integration.State) -> Color {
        switch s {
        case .connected: return .green
        case .connectedWithoutHooks: return .yellow
        case .notConnected: return .secondary
        case .notInstalled: return .secondary
        }
    }
}

struct ConnectToolsSheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Connect tools").font(.title2.bold())
            Text("Conductor's MCP server and hooks for each coding tool on this Mac.")
                .foregroundStyle(.secondary)
            ToolsList()
            if let problem = model.problem {
                Text(problem).foregroundStyle(.red).font(.callout).textSelection(.enabled)
            }
            HStack {
                if let busy = model.busy {
                    ProgressView().controlSize(.small)
                    Text(busy).font(.callout)
                }
                Spacer()
                Button("Refresh") { Task { await model.loadDoctor() } }
                Button("Connect All") { Task { await model.connectTools() } }
                    .disabled(model.busy != nil)
                Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
            }
        }
        .padding(24)
        .frame(width: 560)
        .task { await model.loadDoctor() }
    }
}
