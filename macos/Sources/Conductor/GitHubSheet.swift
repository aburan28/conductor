import AppKit
import ConductorKit
import SwiftUI

/// GitHub: create the app, install it, link repositories, and see when it last looked
/// (`conductor github status / setup / link`).
@MainActor
struct GitHubPanel: View {
    @EnvironmentObject var model: AppModel
    @State private var org = ""

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            if let st = model.github {
                switch st.stage {
                case .noApp:
                    Text("No GitHub App is connected. Creating one takes two clicks on GitHub: the page opens in your browser, where you are signed in.")
                        .fixedSize(horizontal: false, vertical: true)
                    HStack {
                        TextField("Organization (optional)", text: $org).frame(maxWidth: 240)
                        Button("Create GitHub App") { Task { await model.createGitHubApp(org: org) } }
                            .disabled(model.busy != nil)
                    }
                    Text("Leave the organization empty to own the app with your personal account.")
                        .font(.caption).foregroundStyle(.secondary)
                case .notInstalled, .installed:
                    if let app = st.app {
                        LabeledContent("App") { Text("\(app.name ?? app.slug ?? "Conductor") (\(app.owner ?? "?"))") }
                    }
                    LabeledContent("Delivery") {
                        Text([st.mode, st.lastPoll.map { "last poll \($0)" }].compactMap { $0 }.joined(separator: ", "))
                    }
                    if st.stage == .notInstalled {
                        HStack {
                            Text("Installed nowhere yet.")
                            Button("Install on Repositories…") { model.installGitHubApp() }
                        }
                    } else {
                        LabeledContent("Installed on") { Text(st.installations.map(\.account).joined(separator: ", ")) }
                    }
                    if let e = st.installationsError, !e.isEmpty {
                        Text(e).font(.callout).foregroundStyle(.orange)
                    }
                    if let e = st.lastError, !e.isEmpty {
                        Text("Last error: \(e)").font(.callout).foregroundStyle(.orange)
                    }
                    ForEach(Array(st.permissionGaps.enumerated()), id: \.offset) { _, gap in
                        HStack {
                            Text("Needs \(gap.missing ?? "a permission"): \(gap.fix ?? "")").font(.callout)
                            if let s = gap.url, let url = URL(string: s) {
                                Button("Open") { NSWorkspace.shared.open(url) }
                            }
                        }
                    }
                }
                Divider()
                Text("Linked repositories").font(.headline)
                if st.linked.isEmpty {
                    Text("None yet.").foregroundStyle(.secondary)
                } else {
                    ForEach(Array(st.linked.enumerated()), id: \.offset) { _, link in
                        HStack {
                            Text(link["project"] ?? "").frame(width: 160, alignment: .leading)
                            Text(link["repository"] ?? "").foregroundStyle(.secondary)
                        }
                    }
                }
                if st.stage != .noApp {
                    Menu("Link a Repository") {
                        ForEach(model.settings.repositories, id: \.self) { path in
                            Button(path) { Task { await model.linkGitHub(repository: URL(fileURLWithPath: path)) } }
                        }
                        Divider()
                        Button("Choose Folder…") { chooseAndLink() }
                    }
                    .frame(maxWidth: 220)
                }
            } else {
                HStack {
                    ProgressView().controlSize(.small)
                    Text("Asking the control plane about GitHub…")
                }
            }
            HStack {
                Button("Refresh") { Task { await model.loadGitHub() } }
                if let busy = model.busy {
                    ProgressView().controlSize(.small)
                    Text(busy).font(.callout)
                }
            }
        }
        .task { await model.loadGitHub() }
    }

    private func chooseAndLink() {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.prompt = "Link"
        panel.message = "Choose the checkout whose origin is the GitHub repository."
        guard panel.runModal() == .OK, let url = panel.url else { return }
        Task { await model.linkGitHub(repository: url) }
    }
}

struct GitHubSheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("GitHub").font(.title2.bold())
            GitHubPanel()
            if let problem = model.problem {
                Text(problem).foregroundStyle(.red).font(.callout).textSelection(.enabled)
            }
            HStack {
                Spacer()
                Button("Done") { dismiss() }.keyboardShortcut(.defaultAction)
            }
        }
        .padding(24)
        .frame(width: 560)
    }
}
