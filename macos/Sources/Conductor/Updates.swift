import AppKit
import Combine
import Sparkle
import SwiftUI

/// Check for Updates… and the daily check, by Sparkle (pinned to an exact version in
/// Package.swift).
///
/// An update is installed only if its EdDSA signature matches `SUPublicEDKey` in Info.plist,
/// which packaging/build-dmg.sh writes from `SPARKLE_PUBLIC_ED_KEY` at release time. A build
/// without the key, a checkout's or any unsigned one, has nothing to verify an update
/// against, so it never starts Sparkle and the menu item says so.
@MainActor
final class Updates: ObservableObject {
    private let controller: SPUStandardUpdaterController?
    private var observations: Set<AnyCancellable> = []

    @Published private(set) var canCheck = true

    var isEnabled: Bool { controller != nil }

    var automaticallyChecks: Bool {
        get { controller?.updater.automaticallyChecksForUpdates ?? false }
        set {
            objectWillChange.send()
            controller?.updater.automaticallyChecksForUpdates = newValue
        }
    }

    var lastCheck: Date? { controller?.updater.lastUpdateCheckDate }

    init() {
        let key = (Bundle.main.object(forInfoDictionaryKey: "SUPublicEDKey") as? String)?
            .trimmingCharacters(in: .whitespacesAndNewlines) ?? ""
        guard !key.isEmpty else {
            controller = nil
            return
        }
        controller = SPUStandardUpdaterController(startingUpdater: true, updaterDelegate: nil, userDriverDelegate: nil)
        controller?.updater.publisher(for: \.canCheckForUpdates)
            .receive(on: RunLoop.main)
            .sink { [weak self] in self?.canCheck = $0 }
            .store(in: &observations)
    }

    func check() {
        if let controller {
            controller.checkForUpdates(nil)
            return
        }
        let alert = NSAlert()
        alert.messageText = "This build cannot check for updates"
        alert.informativeText = "It was built without an update-signing key (SUPublicEDKey), so there is nothing to verify a download against. Install the newest Conductor from its disk image."
        alert.addButton(withTitle: "OK")
        alert.runModal()
    }

    /// The version a person should read: build-dmg.sh stamps the release's; a checkout's
    /// build says 0.0.0.
    static var versionLabel: String {
        let v = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "0.0.0"
        return v == "0.0.0" ? "development build" : v
    }
}

struct CheckForUpdatesButton: View {
    @ObservedObject var updates: Updates

    var body: some View {
        Button("Check for Updates…") { updates.check() }
            .disabled(!updates.canCheck)
    }
}

struct UpdatesSettings: View {
    @ObservedObject var updates: Updates

    var body: some View {
        Form {
            LabeledContent("Conductor \(Updates.versionLabel)") {
                Button("Check Now") { updates.check() }.disabled(!updates.canCheck)
            }
            if updates.isEnabled {
                Toggle("Check for updates automatically", isOn: Binding(
                    get: { updates.automaticallyChecks },
                    set: { updates.automaticallyChecks = $0 }
                ))
                if let last = updates.lastCheck {
                    LabeledContent("Last checked") { Text(last, style: .relative) }
                }
                Text("An update installs only if its signature matches this app's key.")
                    .font(.callout).foregroundStyle(.secondary)
            } else {
                Text("This build has no update-signing key, so it does not install updates. Install the newest Conductor from its disk image.")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            }
        }
        .formStyle(.grouped)
    }
}
