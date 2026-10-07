import AppKit
import ConductorKit
import SwiftUI

/// The menu bar popover: who is live, what is contested, work offered to you, pull requests
/// and their Conductor checks, and the quick actions.
@MainActor
struct MenuBarView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        VStack(alignment: .leading, spacing: 10) {
            header
            Divider()
            if !model.daemonUp {
                Text(model.phase == .running || model.phase == .idle ? "The control plane is not running." : model.phase.label)
                    .foregroundStyle(.secondary)
                Button("Start Conductor") { Task { await model.restartServices() } }
            } else if !model.signedIn {
                Text("Not signed in.").foregroundStyle(.secondary)
                Button("Open Conductor") { showWindow(WindowID.dashboard, openWindow: openWindow) }
            } else {
                sections
            }
            if let problem = model.databaseProblem {
                Label(problem, systemImage: "externaldrive.badge.exclamationmark")
                    .font(.callout).foregroundStyle(.orange).lineLimit(3)
                    .help("Database archiving to the storage bucket (Settings → Storage)")
            }
            Divider()
            actions
        }
        .padding(14)
        .frame(width: 340)
        .task { await model.refresh() }
    }

    private var header: some View {
        HStack {
            Image(nsImage: StatusDotImage.make(model.dot))
            VStack(alignment: .leading, spacing: 2) {
                Text("Conductor").font(.headline)
                Text(subtitle).font(.caption).foregroundStyle(.secondary)
            }
            Spacer()
            if let busy = model.busy {
                ProgressView().controlSize(.small).help(busy)
            }
        }
    }

    private var subtitle: String {
        var parts: [String] = []
        if let p = model.project { parts.append(p) }
        if let h = model.signIn.handle { parts.append("signed in as \(h)") }
        return parts.isEmpty ? model.settings.endpoint : parts.joined(separator: " · ")
    }

    @ViewBuilder private var sections: some View {
        let live = model.status?.liveSessions ?? []
        let conflicts = model.status?.openConflicts ?? []
        PopoverSection(title: "Live now", count: live.count) {
            ForEach(live.prefix(8)) { s in
                HStack(spacing: 6) {
                    Text(s.principal).fontWeight(.medium)
                    Text(s.harness ?? "").foregroundStyle(.secondary)
                    Spacer()
                    Text(s.taskRef ?? s.state ?? "").font(.caption).foregroundStyle(.secondary)
                }
                .font(.callout)
            }
        }
        if !conflicts.isEmpty {
            PopoverSection(title: "Conflicts", count: conflicts.count) {
                ForEach(conflicts.prefix(5)) { c in
                    VStack(alignment: .leading, spacing: 3) {
                        HStack {
                            Text("\(c.mine?.taskRef ?? "?") ↔ \(c.other?.taskRef ?? "?")").fontWeight(.medium)
                            if let owner = c.other?.owner { Text(owner).foregroundStyle(.secondary) }
                            Spacer()
                            Text(c.suggestionLabel).font(.caption.bold())
                                .padding(.horizontal, 6).padding(.vertical, 2)
                                .background(Capsule().fill(Color.orange.opacity(0.2)))
                        }
                        if let reason = c.reason, !reason.isEmpty {
                            Text(reason).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                        }
                        HStack {
                            Button("Acknowledge") { Task { await model.acknowledge(c) } }
                            Button("Open") {
                                model.reloadDashboard()
                                showWindow(WindowID.dashboard, openWindow: openWindow)
                            }
                        }
                        .controlSize(.small)
                    }
                    .font(.callout)
                }
            }
        }
        if !model.offers.isEmpty {
            PopoverSection(title: "Offered to you", count: model.offers.count) {
                ForEach(model.offers) { offer in
                    HStack {
                        VStack(alignment: .leading) {
                            Text(offer.taskRef ?? "task").fontWeight(.medium)
                            if let r = offer.rationale { Text(r).font(.caption).foregroundStyle(.secondary).lineLimit(2) }
                        }
                        Spacer()
                        Button("Accept") { Task { await model.respond(to: offer, accept: true) } }
                        Button("Decline") { Task { await model.respond(to: offer, accept: false) } }
                    }
                    .controlSize(.small)
                    .font(.callout)
                }
            }
        }
        if !model.pullRequests.isEmpty {
            PopoverSection(title: "Pull requests", count: model.pullRequests.count) {
                ForEach(model.pullRequests.prefix(6)) { pr in
                    HStack {
                        Text(pr.taskRef).fontWeight(.medium)
                        Text(pr.title ?? "").lineLimit(1).foregroundStyle(.secondary)
                        Spacer()
                        Text(pr.label).font(.caption)
                            .foregroundStyle(pr.needsAttention ? Color.orange : Color.secondary)
                        Button {
                            if let url = URL(string: pr.url) { NSWorkspace.shared.open(url) }
                        } label: { Image(systemName: "arrow.up.right.square") }
                            .buttonStyle(.borderless)
                    }
                    .font(.callout)
                }
            }
        }
    }

    private var actions: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Button("Pause All") { Task { await model.pauseAll() } }
                    .help("Checkpoint and stop every agent terminal (conductor pause)")
                Button("Resume All") { Task { await model.resumeAll() } }
                    .help("Wake every paused agent (conductor resume)")
            }
            .disabled(model.busy != nil)
            Button("Open Dashboard") { showWindow(WindowID.dashboard, openWindow: openWindow) }
            Button("Invite Someone…") {
                model.present(.invite)
                showWindow(WindowID.dashboard, openWindow: openWindow)
            }
            .disabled(!model.signedIn)
            Button("Checkpoints…") { showWindow(WindowID.checkpoints, openWindow: openWindow) }
            HStack {
                Button("Settings…") { openSettings() }
                Spacer()
                Button("Quit") { NSApp.terminate(nil) }
            }
        }
        .buttonStyle(.link)
    }
}

private struct PopoverSection<Content: View>: View {
    let title: String
    let count: Int
    @ViewBuilder let content: () -> Content

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Text(title).font(.subheadline.bold())
                Text("\(count)").font(.caption).foregroundStyle(.secondary)
            }
            if count == 0 {
                Text("Nobody yet.").font(.callout).foregroundStyle(.secondary)
            } else {
                content()
            }
        }
    }
}
