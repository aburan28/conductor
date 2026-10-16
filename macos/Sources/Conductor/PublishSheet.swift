import AppKit
import ConductorKit
import SwiftUI

/// Remote Access: the get-up-and-running checklist for publishing this Mac's control
/// plane over Tailscale (`conductor publish`, or the raw serve/funnel commands), with the
/// resulting tailnet/public URL when it can be computed (docs/REMOTE_ACCESS.md).
@MainActor
struct PublishSheet: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) private var dismiss

    @State private var tailscaleFound = true
    @State private var magicDNS: String?
    @State private var published = false
    @State private var funnel = false
    @State private var working = false

    private var publicURL: String? {
        guard let magicDNS, published else { return nil }
        return "https://" + magicDNS + "/"
    }

    var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            Text("Remote Access").font(.title2.bold())
            Text("Let teammates and agents on other machines reach this Mac's control plane over Tailscale, with nothing exposed from your home network. Publish to your tailnet only, or — with Funnel — to the whole internet.")
                .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)

            step(1, title: "Install Tailscale",
                 done: tailscaleFound,
                 detail: "From the App Store or tailscale.com/download. The menu-bar app carries the command this sheet runs.") {
                Button("Open tailscale.com/download") { NSWorkspace.shared.open(URL(string: "https://tailscale.com/download")!) }
            }

            step(2, title: "Sign in to your tailnet",
                 done: magicDNS != nil,
                 detail: magicDNS.map { "Connected as \($0)." }
                     ?? "Open Tailscale from the menu bar and sign in (or run `tailscale up`); MagicDNS and HTTPS must be on.") {}

            step(3, title: "Publish",
                 done: published,
                 detail: published ? "Conductor is published. Stop with `tailscale funnel off` or `tailscale serve reset`."
                     : "One command does it: publish, then verify the route (a re-run funnel command can clobber it).") {
                Toggle("Also on the public internet (Funnel)", isOn: $funnel)
                    .disabled(working)
                CommandRow(command: funnel ? "conductor publish --funnel" : "conductor publish")
                if working { ProgressView().controlSize(.small) } else {
                    Button(published ? "Publish Again" : "Publish Now") { Task { await publish() } }
                        .disabled(magicDNS == nil)
                }
                DisclosureGroup("The raw commands") {
                    CommandRow(command: rawServeCommand)
                    CommandRow(command: rawFunnelCommand)
                    Text("Always pass the full URL form: `tailscale funnel --bg 8443` would treat the number as the backend and re-point the public route at plain HTTP. On macOS the CLI is at /Applications/Tailscale.app/Contents/MacOS/Tailscale; a plain symlink to it crashes, so call it by path or through PATH.")
                        .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                }
            }

            if let url = publicURL {
                GroupBox {
                    VStack(alignment: .leading, spacing: 6) {
                        LabeledContent("Dashboard") { Text(url).textSelection(.enabled) }
                        LabeledContent("MCP") { Text(url + "mcp").textSelection(.enabled) }
                        Text("Set the same URL as the Public URL in Settings (conductord --public-url) and pass it to invites: `conductor invite <handle> --endpoint \(url)`.")
                            .font(.caption).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    }.padding(4)
                }
            }

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
        .task { await check() }
    }

    // MARK: - steps

    @ViewBuilder
    private func step<Content: View>(_ number: Int, title: String, done: Bool, detail: String,
                                     @ViewBuilder content: () -> Content) -> some View {
        HStack(alignment: .top, spacing: 10) {
            Image(systemName: done ? "checkmark.circle.fill" : "\(number).circle")
                .foregroundStyle(done ? Color.green : Color.secondary)
            VStack(alignment: .leading, spacing: 6) {
                Text(title).font(.headline)
                Text(detail).font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                content()
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    private var rawServeCommand: String {
        "tailscale " + Tailscale.publishServeArguments(backend: backend).joined(separator: " ")
    }

    private var rawFunnelCommand: String {
        "tailscale " + Tailscale.publishFunnelArguments(backend: backend).joined(separator: " ")
    }

    /// The loopback daemon's own scheme decides the backend form: a self-signed HTTPS
    /// daemon is reached with https+insecure (that hop only), a plain HTTP one with http.
    private var backend: String {
        let tls = model.settings.endpoint.hasPrefix("https")
        return Tailscale.publishBackend(port: model.settings.daemonPort, tls: tls)
    }

    // MARK: - actions

    private func tailscale(_ args: [String]) async -> CommandResult? {
        guard let exe = Tailscale.locate() else { return nil }
        return try? await ProcessRunner().run(CommandSpec(exe, args, environment: model.commandEnvironment))
    }

    private func check() async {
        tailscaleFound = Tailscale.locate() != nil
        if let status = await tailscale(Tailscale.statusArguments), status.succeeded {
            magicDNS = Tailscale.magicDNSName(statusJSON: status.stdout)
            if magicDNS != nil, let serve = await tailscale(Tailscale.serveStatusArguments), serve.succeeded {
                published = Tailscale.isServing(port: model.settings.daemonPort, serveStatusJSON: serve.stdout)
            }
        }
    }

    private func publish() async {
        working = true
        defer { working = false }
        model.problem = nil
        guard let name = magicDNS else {
            model.problem = "Tailscale is not connected yet; sign in from its menu-bar app first."
            return
        }
        for args in [Tailscale.publishServeArguments(backend: backend)]
            + (funnel ? [Tailscale.publishFunnelArguments(backend: backend)] : []) {
            guard let r = await tailscale(args) else {
                model.problem = "The tailscale command was not found."
                return
            }
            if !r.succeeded {
                model.problem = r.failureMessage("tailscale \(args.first ?? "")")
                return
            }
        }
        // Re-read the serve config instead of trusting exit codes: a re-run funnel
        // command can clobber the root route.
        if let serve = await tailscale(Tailscale.serveStatusArguments), serve.succeeded {
            published = Tailscale.isServing(port: model.settings.daemonPort, serveStatusJSON: serve.stdout)
            if !published {
                model.problem = "tailscale did not leave the root route proxying to \(backend); run `tailscale serve status` and re-point it."
                return
            }
        }
        if funnel, let serve = await tailscale(Tailscale.serveStatusArguments), serve.succeeded,
           !Tailscale.funnelOn(hostPort: name + ":443", serveStatusJSON: serve.stdout) {
            model.problem = "Funnel did not turn on for \(name); check that Funnel and HTTPS certificates are enabled for your tailnet."
            return
        }
        await check()
    }
}

/// One command, shown as monospaced text with a Copy button beside it.
private struct CommandRow: View {
    let command: String

    var body: some View {
        HStack {
            Text(command).font(.system(.callout, design: .monospaced)).textSelection(.enabled).lineLimit(2)
            Spacer()
            Button("Copy") {
                NSPasteboard.general.clearContents()
                NSPasteboard.general.setString(command, forType: .string)
            }
        }
    }
}
