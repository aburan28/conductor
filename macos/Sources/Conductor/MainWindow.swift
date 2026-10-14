import AppKit
import ConductorKit
import SwiftUI

/// The main window: the first-run flow until it is finished, then the dashboard.
struct MainWindow: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Group {
            if model.settings.onboardingComplete {
                DashboardView()
            } else {
                OnboardingView()
            }
        }
        .sheet(item: $model.sheet) { sheet in
            switch sheet {
            case .invite: InviteSheet()
            case .tools: ConnectToolsSheet()
            case .github: GitHubSheet()
            case .join: JoinSheet()
            }
        }
        .onChange(of: model.windowRequest) { request in
            guard let request, request != WindowID.dashboard else { return }
            model.windowRequest = nil
            showWindow(request, openWindow: openWindow)
        }
    }
}

struct DashboardView: View {
    @EnvironmentObject var model: AppModel
    @StateObject private var browser = Browser()

    var body: some View {
        ZStack {
            if model.daemonUp {
                WebView(browser: browser)
                    .onAppear { browser.show(model.dashboardURL) }
                    .onChange(of: model.dashboardURL) { url in browser.show(url) }
                    .onChange(of: model.dashboardReloads) { _ in browser.reload() }
                if let failure = browser.failure {
                    Unreachable(message: failure) { browser.reload() }
                }
            } else {
                Unreachable(message: model.phase == .running || model.phase == .idle
                            ? "The control plane is not answering at \(model.settings.endpoint)."
                            : model.phase.label) {
                    Task { await model.restartServices() }
                }
            }
        }
        .toolbar {
            ToolbarItemGroup(placement: .primaryAction) {
                if let busy = model.busy {
                    ProgressView().controlSize(.small).help(busy)
                }
                if model.projects.count > 1 {
                    Picker("Project", selection: Binding(
                        get: { model.settings.project },
                        set: { model.settings.project = $0; model.saveSettings(); model.startStreaming(); Task { await model.refresh() } }
                    )) {
                        ForEach(model.projects, id: \.slug) { Text($0.slug).tag($0.slug) }
                    }
                    .help("The project the menu bar follows")
                }
                Button { model.present(.invite) } label: { Label("Invite", systemImage: "person.badge.plus") }
                    .help("Invite someone by text")
                    .disabled(!model.signedIn)
                Button { model.requestWindow(WindowID.checkpoints) } label: { Label("Checkpoints", systemImage: "clock.arrow.circlepath") }
                    .help("Continue a session under another login or in another tool")
                Button { browser.reload() } label: { Label("Reload", systemImage: "arrow.clockwise") }
            }
        }
        .navigationTitle(model.project.map { "Conductor — \($0)" } ?? "Conductor")
    }
}

struct Unreachable: View {
    let message: String
    let retry: () -> Void

    var body: some View {
        VStack(spacing: 12) {
            Image(systemName: "bolt.horizontal.circle").font(.system(size: 40)).foregroundStyle(.secondary)
            Text(message).multilineTextAlignment(.center).frame(maxWidth: 480)
            Button("Try Again", action: retry)
        }
        .padding(40)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color(nsColor: .windowBackgroundColor))
    }
}

/// `conductor://join#…` opened on this Mac: confirm, then join and connect the tools.
struct JoinSheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Join a Conductor swarm").font(.title2.bold())
            if let j = model.pendingJoin {
                LabeledContent("Conductor") { Text(j.endpoint).textSelection(.enabled) }
                if let p = j.project { LabeledContent("Project") { Text(p) } }
                Text("Joining saves this login for the conductor command and connects the coding tools on this Mac (Claude Code, Codex, OpenCode, …) so they show Conductor's MCP tools.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                if model.settings.onboardingComplete == false && !model.settings.isAttached {
                    Text("This Mac will use that Conductor; its own control plane keeps running for anything you set up here.")
                        .font(.callout).foregroundStyle(.secondary)
                }
            }
            if let problem = model.problem {
                Text(problem).foregroundStyle(.red).font(.callout).textSelection(.enabled)
            }
            HStack {
                if let busy = model.busy {
                    ProgressView().controlSize(.small)
                    Text(busy).font(.callout)
                }
                Spacer()
                Button("Cancel") { model.pendingJoin = nil; dismiss() }
                    .keyboardShortcut(.cancelAction)
                Button("Join") {
                    guard let j = model.pendingJoin else { return }
                    Task { await model.join(j) }
                }
                .keyboardShortcut(.defaultAction)
                .disabled(model.pendingJoin == nil || model.busy != nil)
            }
        }
        .padding(24)
        .frame(width: 480)
    }
}
