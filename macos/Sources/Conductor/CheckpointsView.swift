import AppKit
import ConductorKit
import SwiftUI

/// Checkpoints of every session on this Mac (`conductor checkpoint list --json`), newest
/// first, and three ways to continue each: here, under another login, in another tool. The
/// resume opens in Terminal, through the same opener `conductor resume` uses.
@MainActor
struct CheckpointsView: View {
    @EnvironmentObject var model: AppModel
    @State private var otherAccount = ""
    @State private var askingAccountFor: CheckpointManifest?

    var body: some View {
        VStack(spacing: 0) {
            if let error = model.checkpointError {
                Text(error).foregroundStyle(.red).padding().textSelection(.enabled)
            }
            if model.checkpointGroups.isEmpty && model.checkpointError == nil {
                VStack(spacing: 8) {
                    Image(systemName: "clock.arrow.circlepath").font(.system(size: 36)).foregroundStyle(.secondary)
                    Text("No checkpoints on this Mac yet.")
                    Text("`conductor wrap` takes them automatically; `conductor checkpoint capture` takes one now.")
                        .font(.callout).foregroundStyle(.secondary)
                }
                .frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                List {
                    ForEach(model.checkpointGroups) { group in
                        CheckpointRow(m: group.latest, older: group.checkpoints.count - 1,
                                      accounts: model.accounts(for: group.latest.harness),
                                      resume: { target in Task { await model.resume(group.latest, target) } },
                                      askAccount: { askingAccountFor = group.latest })
                    }
                }
                .listStyle(.inset)
            }
            if let problem = model.problem {
                Text(problem).foregroundStyle(.red).font(.callout).padding(8).textSelection(.enabled)
            }
        }
        .toolbar {
            ToolbarItem {
                Button { Task { await model.loadCheckpoints() } } label: { Label("Refresh", systemImage: "arrow.clockwise") }
            }
        }
        .navigationTitle("Checkpoints")
        .task { await model.loadCheckpoints() }
        .sheet(item: $askingAccountFor) { m in
            VStack(alignment: .leading, spacing: 12) {
                Text("Continue under another login").font(.headline)
                Text("A name from ~/.\(m.harness)-<name> or ~/.conductor/accounts/\(m.harness)/<name>. A new name starts a fresh login there; the harness asks you to sign in.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                TextField("Account name", text: $otherAccount)
                HStack {
                    Spacer()
                    Button("Cancel") { askingAccountFor = nil }.keyboardShortcut(.cancelAction)
                    Button("Continue") {
                        let name = otherAccount.trimmingCharacters(in: .whitespaces)
                        askingAccountFor = nil
                        Task { await model.resume(m, .account(name)) }
                    }
                    .keyboardShortcut(.defaultAction)
                    .disabled(otherAccount.trimmingCharacters(in: .whitespaces).isEmpty)
                }
            }
            .padding(20)
            .frame(width: 420)
        }
    }
}

@MainActor
private struct CheckpointRow: View {
    let m: CheckpointManifest
    let older: Int
    let accounts: [String]
    let resume: (ResumeTarget) -> Void
    let askAccount: () -> Void

    var body: some View {
        HStack(alignment: .top, spacing: 12) {
            VStack(alignment: .leading, spacing: 4) {
                HStack(spacing: 6) {
                    Text(m.displayTitle).font(.headline).lineLimit(1)
                    if m.isUsageLimit {
                        Text("usage limit").font(.caption.bold())
                            .padding(.horizontal, 6).padding(.vertical, 1)
                            .background(Capsule().fill(Color.orange.opacity(0.25)))
                    }
                }
                Text(details).font(.caption).foregroundStyle(.secondary)
                if let note = m.note, !note.isEmpty {
                    Text(note).font(.caption).foregroundStyle(.secondary).lineLimit(2)
                }
            }
            Spacer()
            HStack {
                Button("Continue Here") { resume(.here) }
                Menu("Under Account") {
                    ForEach(accounts, id: \.self) { name in
                        Button(name) { resume(.account(name)) }
                    }
                    if !accounts.isEmpty { Divider() }
                    Button("Other…") { askAccount() }
                }
                .fixedSize()
                Menu("Continue In") {
                    ForEach(Harness.allCases.filter { $0 != Harness.normalize(m.harness) }) { h in
                        Button(h.title) { resume(.harness(h)) }
                    }
                }
                .fixedSize()
            }
            .controlSize(.small)
        }
        .padding(.vertical, 4)
    }

    private var details: String {
        var parts = [Harness.normalize(m.harness)?.title ?? m.harness, m.shortID]
        if let d = m.createdDate {
            parts.append(RelativeDateTimeFormatter().localizedString(for: d, relativeTo: Date()))
        }
        if m.turns > 0 { parts.append("\(m.turns) turns") }
        if let b = m.repo?.branch { parts.append(b) }
        if older > 0 { parts.append("\(older) older") }
        return parts.joined(separator: " · ")
    }
}
