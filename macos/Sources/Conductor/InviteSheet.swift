import AppKit
import ConductorKit
import SwiftUI

/// Invite someone: a handle, a role and an expiry; the members API mints their token; the
/// link goes to Messages, Mail or AirDrop through the share sheet with one line of text.
@MainActor
struct InviteSheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss

    @State private var handle = ""
    @State private var role: InviteRole = .contributor
    @State private var expiry: InviteExpiry = .week
    @State private var reachability: InviteReachability?
    @State private var link: String?
    @State private var appLink: String?
    @State private var note: String?
    @State private var working = false

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Invite someone").font(.title2.bold())
            Form {
                TextField("Their handle", text: $handle, prompt: Text("rachel"))
                Picker("Role", selection: $role) {
                    ForEach(InviteRole.allCases) { Text($0.title).tag($0) }
                }
                Picker("Link works for", selection: $expiry) {
                    ForEach(InviteExpiry.allCases) { Text($0.title).tag($0) }
                }
            }
            .disabled(link != nil)

            reachabilityView

            if let link {
                GroupBox {
                    VStack(alignment: .leading, spacing: 8) {
                        Text(link).font(.system(.callout, design: .monospaced)).textSelection(.enabled).lineLimit(3)
                        Text("The link carries a sign-in token: send it once, over a channel you trust. It is shown only now.")
                            .font(.caption).foregroundStyle(.secondary)
                        HStack {
                            ShareButton(title: "Share…", items: { [InviteLink.shareText(link) + appLinkLine] })
                                .fixedSize()
                            Button("Copy") {
                                NSPasteboard.general.clearContents()
                                NSPasteboard.general.setString(link, forType: .string)
                            }
                        }
                    }
                    .padding(4)
                }
            }
            if let note {
                Text(note).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
            if let problem = model.problem {
                Text(problem).foregroundStyle(.red).font(.callout).textSelection(.enabled)
            }
            HStack {
                if working { ProgressView().controlSize(.small) }
                Spacer()
                Button(link == nil ? "Cancel" : "Done") { dismiss() }
                    .keyboardShortcut(.cancelAction)
                if link == nil {
                    Button("Create Link") { Task { await create() } }
                        .keyboardShortcut(.defaultAction)
                        .disabled(handle.trimmingCharacters(in: .whitespaces).isEmpty || working)
                }
            }
        }
        .padding(24)
        .frame(width: 540)
        .task { await checkReachability() }
    }

    /// A second line for someone with the app: the same invite as `conductor://join`, which
    /// opens the app and connects their tools.
    private var appLinkLine: String {
        guard let appLink else { return "" }
        return "\n\nWith Conductor for Mac installed: \(appLink)"
    }

    @ViewBuilder private var reachabilityView: some View {
        if let r = reachability {
            VStack(alignment: .leading, spacing: 6) {
                Label(r.endpoint == nil ? "Only this Mac can reach the link yet" : "Reachable at \(r.endpoint!)",
                      systemImage: r.endpoint == nil ? "exclamationmark.triangle" : "network")
                    .foregroundStyle(r.endpoint == nil ? Color.orange : Color.primary)
                Text(r.explanation).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                if r.offersTailscale {
                    Button("Share over Tailscale") { Task { await shareOverTailscale() } }
                        .disabled(working)
                }
            }
        } else {
            HStack { ProgressView().controlSize(.small); Text("Checking how others can reach this Mac…").font(.callout) }
        }
    }

    // MARK: - actions

    private func tailscale(_ args: [String]) async -> CommandResult? {
        guard let exe = Tailscale.locate() else { return nil }
        return try? await ProcessRunner().run(CommandSpec(exe, args, environment: model.commandEnvironment))
    }

    private func checkReachability() async {
        var name: String?
        var serving = false
        if let status = await tailscale(Tailscale.statusArguments), status.succeeded {
            name = Tailscale.magicDNSName(statusJSON: status.stdout)
            if name != nil, let serve = await tailscale(Tailscale.serveStatusArguments), serve.succeeded {
                serving = Tailscale.isServing(port: model.settings.daemonPort, serveStatusJSON: serve.stdout)
            }
        }
        reachability = InviteReachability.decide(publicURL: model.settings.publicURL, tailscaleName: name, tailscaleServing: serving)
    }

    /// `tailscale serve --bg <port>`: only after the person clicked, and the sheet says how
    /// to stop.
    private func shareOverTailscale() async {
        working = true
        defer { working = false }
        guard let r = await tailscale(Tailscale.serveArguments(port: model.settings.daemonPort)) else {
            model.problem = "The tailscale command was not found."
            return
        }
        if !r.succeeded {
            model.problem = r.failureMessage("tailscale serve")
            return
        }
        await checkReachability()
    }

    private func create() async {
        guard let project = model.project else {
            model.problem = "Pick a repository first: there is no project to invite them to."
            return
        }
        working = true
        defer { working = false }
        model.problem = nil
        do {
            let result = try await model.api.invite(project: project,
                                                    InviteRequest(handle: handle.trimmingCharacters(in: .whitespaces),
                                                                  role: role.rawValue, tokenTTL: expiry.ttl))
            guard let token = result.token, !token.isEmpty else {
                note = result.existingPrincipal == true
                    ? "\(result.handle) already has an account here, so no token was minted. They keep their own login, which now reaches \(project): they run `conductor login --project \(project)`."
                    : (result.note ?? "Added \(result.handle).")
                return
            }
            let endpoint = reachability?.endpoint ?? model.settings.endpoint
            link = InviteLink.web(endpoint: endpoint, project: project, token: token)
            appLink = InviteLink.app(endpoint: endpoint, project: project, token: token)
            if let expires = result.expiresAt { note = "The link expires \(expires)." }
        } catch {
            model.problem = String(describing: error)
        }
    }
}

/// An AppKit button that shows the share sheet (`NSSharingServicePicker`) from itself, so
/// the picker is anchored to the button the person clicked.
struct ShareButton: NSViewRepresentable {
    let title: String
    let items: () -> [Any]

    func makeCoordinator() -> Coordinator { Coordinator(items: items) }

    func makeNSView(context: Context) -> NSButton {
        let button = NSButton(title: title, target: context.coordinator, action: #selector(Coordinator.share(_:)))
        button.bezelStyle = .rounded
        return button
    }

    func updateNSView(_ button: NSButton, context: Context) {
        button.title = title
        context.coordinator.items = items
    }

    final class Coordinator: NSObject {
        var items: () -> [Any]

        init(items: @escaping () -> [Any]) {
            self.items = items
        }

        @objc func share(_ sender: NSButton) {
            let picker = NSSharingServicePicker(items: items())
            picker.show(relativeTo: sender.bounds, of: sender, preferredEdge: .minY)
        }
    }
}
