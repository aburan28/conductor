import AppKit
import ConductorKit
import ServiceManagement
import SwiftUI

struct SettingsView: View {
    @ObservedObject var updates: Updates

    var body: some View {
        TabView {
            GeneralSettings()
                .tabItem { Label("General", systemImage: "gearshape") }
            StorageSettingsView()
                .tabItem { Label("Storage", systemImage: "externaldrive.connected.to.line.below") }
            UpdatesSettings(updates: updates)
                .tabItem { Label("Updates", systemImage: "arrow.down.circle") }
        }
        .frame(width: 640, height: 640)
    }
}

/// Port, start at login, security mode, the data folder, and attaching to a control plane or
/// a database this app does not run.
@MainActor
struct GeneralSettings: View {
    @EnvironmentObject var model: AppModel
    @State private var draft = AppSettings()
    @State private var port = ""
    @State private var databaseURL = ""
    @State private var security: SecurityStatus?
    @State private var securityProblem: String?
    @State private var applying = false

    var body: some View {
        Form {
            Section("Control plane") {
                TextField("Port on 127.0.0.1", text: $port)
                    .help("conductord listens here, on loopback only")
                Toggle("Start Conductor when I log in", isOn: $draft.startAtLogin)
                TextField("Public URL (optional)", text: $draft.publicURL, prompt: Text("https://conductor.example.com"))
                    .help("conductord --public-url: the address teammates reach this Mac at, used in invite links")
            }

            Section {
                if let security {
                    Picker("Sign-in", selection: Binding(
                        get: { security.securityMode },
                        set: { mode in Task { await setSecurity(mode) } }
                    )) {
                        Text("Local: this Mac's owner signs in without a token").tag("local")
                        Text("Enhanced: tokens only, everywhere").tag("enhanced")
                    }
                    .pickerStyle(.radioGroup)
                    .disabled(security.isPinned)
                    if security.isPinned {
                        Text("Pinned by conductord --security-mode.").font(.caption).foregroundStyle(.secondary)
                    }
                } else {
                    Text(securityProblem ?? "Asking the control plane…").foregroundStyle(.secondary)
                }
            } header: {
                Text("Security mode")
            } footer: {
                Text("Local mode lets whoever owns this Mac in from this Mac with no token. Use enhanced mode on a Mac other people log in to: loopback is shared by every account on it. Turning enhanced on revokes every token local sign-in issued.")
                    .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }

            Section("Data") {
                LabeledContent("Data folder") {
                    HStack {
                        Text(model.paths.support.path).truncationMode(.middle).lineLimit(1).textSelection(.enabled)
                        Button("Show in Finder") { NSWorkspace.shared.activateFileViewerSelecting([model.paths.support]) }
                    }
                }
                LabeledContent("Logs") {
                    Button("Show in Finder") { NSWorkspace.shared.activateFileViewerSelecting([model.paths.logs]) }
                }
            }

            Section {
                TextField("Attach to a control plane", text: $draft.attachEndpoint, prompt: Text("https://conductor.team"))
                    .help("Show a Conductor this app does not run; nothing is started on this Mac")
                Toggle("Use a database I run", isOn: $draft.usesExternalDatabase)
                if draft.usesExternalDatabase {
                    SecureField("Database URL", text: $databaseURL, prompt: Text("postgres://user:password@host/conductor"))
                        .help("Kept in the Keychain, never in a file this app writes; conductord gets it in DATABASE_URL")
                }
            } header: {
                Text("Attach instead")
            } footer: {
                Text("Leave both empty to run Conductor and its private Postgres on this Mac.")
                    .font(.caption).foregroundStyle(.secondary)
            }

            Section {
                HStack {
                    Button("Apply and Restart") { Task { await apply() } }
                        .keyboardShortcut(.defaultAction)
                        .disabled(applying)
                    if applying { ProgressView().controlSize(.small) }
                    Spacer()
                    Button("Stop Conductor") { Task { await model.stopServices() } }
                }
                if let problem = model.problem {
                    Text(problem).foregroundStyle(.red).font(.caption).textSelection(.enabled)
                }
            }
        }
        .formStyle(.grouped)
        .task {
            draft = model.settings
            port = String(model.settings.daemonPort)
            if model.settings.usesExternalDatabase {
                databaseURL = (try? model.secrets.read(service: KeychainNames.databaseService, account: KeychainNames.databaseAccount)) ?? ""
            }
            await loadSecurity()
        }
    }

    private func loadSecurity() async {
        guard let r = try? await model.runConductor(ConductorCommands.securityStatus) else { return }
        if r.succeeded, let s = try? JSONDecoder().decode(SecurityStatus.self, from: r.stdout) {
            security = s
            securityProblem = nil
        } else {
            securityProblem = r.failureMessage("conductor security")
        }
    }

    private func setSecurity(_ mode: String) async {
        await model.perform("Switching to \(mode) mode…", ConductorCommands.setSecurity(mode))
        await loadSecurity()
        if mode == "local" { await model.signInIfNeeded() }
    }

    private func apply() async {
        applying = true
        defer { applying = false }
        guard let p = Int(port.trimmingCharacters(in: .whitespaces)), p > 0, p < 65536 else {
            model.problem = "The port must be a number from 1 to 65535."
            return
        }
        if draft.usesExternalDatabase {
            let dsn = databaseURL.trimmingCharacters(in: .whitespacesAndNewlines)
            guard !dsn.isEmpty else {
                model.problem = "Enter the database URL, or turn off \"Use a database I run\"."
                return
            }
            do {
                try model.secrets.write(dsn, service: KeychainNames.databaseService, account: KeychainNames.databaseAccount,
                                        label: "Conductor database URL", trustedPaths: [])
            } catch {
                model.problem = String(describing: error)
                return
            }
        }
        // The app itself at login, beside the agents' RunAtLoad.
        do {
            if draft.startAtLogin {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
        } catch {
            // Not fatal: the agents still start at login; only the menu bar item waits.
        }
        // Only the fields this form edits are written back. The draft was copied when the form
        // appeared, so assigning it whole would revert anything changed since (onboarding,
        // repositories, the project, the skip flags).
        model.settings.daemonPort = p
        model.settings.startAtLogin = draft.startAtLogin
        model.settings.publicURL = draft.publicURL
        model.settings.attachEndpoint = draft.attachEndpoint
        model.settings.usesExternalDatabase = draft.usesExternalDatabase
        model.saveSettings()
        await model.restartServices()
    }
}
