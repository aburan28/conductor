import AppKit
import ConductorKit
import SwiftUI

/// The app's delegate: links (`conductor://join#…`) and quitting.
///
/// Links arrive through `application(_:open:)`. Every WindowGroup declares that it handles no
/// external events, so SwiftUI does not open a second dashboard window for each link.
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    func application(_ application: NSApplication, open urls: [URL]) {
        for url in urls {
            AppModel.shared.open(url)
        }
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        Notifier.shared.setUp()
        Task { await AppModel.shared.launch() }
    }

    /// Closing the window keeps the app (and its menu bar item) running; the control plane
    /// runs under launchd either way, so quitting stops nothing the person relies on.
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }

    func applicationWillTerminate(_ notification: Notification) {
        AppModel.shared.stopStreaming()
    }
}

@main
struct ConductorApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate
    @StateObject private var model = AppModel.shared
    @StateObject private var updates = Updates()

    var body: some Scene {
        WindowGroup("Conductor", id: WindowID.dashboard) {
            MainWindow()
                .environmentObject(model)
                .frame(minWidth: 900, minHeight: 600)
        }
        .handlesExternalEvents(matching: [])
        .commands {
            CommandGroup(replacing: .newItem) {}
            CommandGroup(after: .appInfo) {
                CheckForUpdatesButton(updates: updates)
            }
            CommandMenu("Conductor") {
                Button("Invite Someone…") { model.present(.invite) }
                    .keyboardShortcut("i", modifiers: [.command, .shift])
                    .disabled(!model.signedIn)
                Button("Connect Tools…") { model.present(.tools) }
                Button("GitHub…") { model.present(.github) }
                    .disabled(!model.signedIn)
                Button("Checkpoints") { model.requestWindow(WindowID.checkpoints) }
                    .keyboardShortcut("k", modifiers: [.command, .shift])
                Divider()
                Button("Pause All Agents") { Task { await model.pauseAll() } }
                Button("Resume All Agents") { Task { await model.resumeAll() } }
                Divider()
                Button("Reload Dashboard") { model.reloadDashboard() }
                    .keyboardShortcut("r", modifiers: [.command])
            }
        }

        Window("Checkpoints", id: WindowID.checkpoints) {
            CheckpointsView()
                .environmentObject(model)
                .frame(minWidth: 720, minHeight: 420)
        }

        MenuBarExtra {
            MenuBarView()
                .environmentObject(model)
        } label: {
            MenuBarLabel()
                .environmentObject(model)
        }
        .menuBarExtraStyle(.window)

        Settings {
            SettingsView(updates: updates)
                .environmentObject(model)
        }
    }
}

enum WindowID {
    static let dashboard = "dashboard"
    static let checkpoints = "checkpoints"
}

/// The menu bar item: a coloured dot. It is also the one view that is always alive, so it
/// opens windows the model asks for (from a notification, or a link) when no other view is
/// on screen to do it.
struct MenuBarLabel: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Image(nsImage: StatusDotImage.make(model.dot))
            .help(model.dot.help)
            .onChange(of: model.windowRequest) { request in
                guard let request else { return }
                model.windowRequest = nil
                showWindow(request, openWindow: openWindow)
            }
    }
}

/// Brings a window to the front, opening it if it is not open. A WindowGroup opens a new
/// window on every `openWindow`, so an existing dashboard is looked for first.
@MainActor
func showWindow(_ id: String, openWindow: OpenWindowAction) {
    NSApp.activate(ignoringOtherApps: true)
    if let existing = NSApp.windows.first(where: { $0.identifier?.rawValue.hasPrefix(id) == true && $0.isVisible }) {
        existing.makeKeyAndOrderFront(nil)
        return
    }
    openWindow(id: id)
}

/// Settings, on macOS 13 and later.
@MainActor
func openSettings() {
    NSApp.activate(ignoringOtherApps: true)
    NSApp.sendAction(Selector(("showSettingsWindow:")), to: nil, from: nil)
}

enum StatusDotImage {
    /// A filled circle in the dot's colour, not a template image, so the menu bar keeps the
    /// colour: green all clear, amber waiting on the person, grey down.
    static func make(_ dot: StatusDot) -> NSImage {
        let color: NSColor
        switch dot {
        case .clear: color = .systemGreen
        case .attention: color = .systemOrange
        case .down: color = .systemGray
        }
        let image = NSImage(size: NSSize(width: 16, height: 16), flipped: false) { rect in
            let circle = NSBezierPath(ovalIn: rect.insetBy(dx: 3, dy: 3))
            color.setFill()
            circle.fill()
            NSColor.black.withAlphaComponent(0.25).setStroke()
            circle.lineWidth = 0.5
            circle.stroke()
            return true
        }
        image.isTemplate = false
        image.accessibilityDescription = dot.help
        return image
    }
}
