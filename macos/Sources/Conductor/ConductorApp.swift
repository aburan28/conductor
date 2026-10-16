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
        // About panel / Dock: prefer the bundled AppIcon.icns from Contents/Resources.
        if let url = Bundle.main.url(forResource: "AppIcon", withExtension: "icns"),
           let icon = NSImage(contentsOf: url) {
            NSApplication.shared.applicationIconImage = icon
        }
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

/// The menu bar item: the Conductor mark tinted by status. It is also the one view that is
/// always alive, so it opens windows the model asks for (from a notification, or a link)
/// when no other view is on screen to do it.
struct MenuBarLabel: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Image(nsImage: StatusDotImage.make(model.dot))
            .accessibilityLabel(model.dot.help)
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

/// Settings on macOS 13. macOS 14 ignores this action from SwiftUI apps and has
/// SettingsLink instead, which `SettingsButton` uses there.
@MainActor
func openSettings() {
    NSApp.activate(ignoringOtherApps: true)
    NSApp.sendAction(Selector(("showSettingsWindow:")), to: nil, from: nil)
}

/// "Settings…" that opens the Settings scene on every supported macOS.
struct SettingsButton: View {
    var title = "Settings…"

    var body: some View {
        if #available(macOS 14.0, *) {
            SettingsLink { Text(title) }
        } else {
            Button(title) { openSettings() }
        }
    }
}

enum StatusDotImage {
    /// The web favicon mark (rounded square + three strokes) tinted by status, not a template
    /// image, so the menu bar keeps the colour: brand green all clear, amber waiting on the
    /// person, grey down. Colour alone is not the only signal — `StatusDot.help` and the
    /// popover header state label carry the same meaning for VoiceOver and colour-blind use.
    static func make(_ dot: StatusDot) -> NSImage {
        let fill: NSColor
        switch dot {
        case .clear:
            // Brand accent from internal/web (#2f6f4e), not systemGreen: matches Dock/Finder.
            fill = NSColor(srgbRed: 0x2F / 255, green: 0x6F / 255, blue: 0x4E / 255, alpha: 1)
        case .attention: fill = .systemOrange
        case .down: fill = .systemGray
        }
        let size = NSSize(width: 18, height: 18)
        // flipped: true so favicon SVG y-down coordinates place the strokes correctly.
        let image = NSImage(size: size, flipped: true) { rect in
            let body = NSBezierPath(roundedRect: rect.insetBy(dx: 1, dy: 1), xRadius: 4, yRadius: 4)
            fill.setFill()
            body.fill()
            NSColor.black.withAlphaComponent(0.22).setStroke()
            body.lineWidth = 0.5
            body.stroke()

            // Favicon strokes on a 32×32 grid, scaled into the body.
            let inset = rect.insetBy(dx: 1, dy: 1)
            let sx = inset.width / 32
            let sy = inset.height / 32
            let stroke = NSBezierPath()
            stroke.lineWidth = max(1.5, 3 * min(sx, sy))
            stroke.lineCapStyle = .round
            let segments: [(CGFloat, CGFloat, CGFloat, CGFloat)] = [
                (9, 10, 23, 10),
                (9, 16, 19, 16),
                (9, 22, 23, 22),
            ]
            for (x0, y0, x1, y1) in segments {
                stroke.move(to: NSPoint(x: inset.minX + x0 * sx, y: inset.minY + y0 * sy))
                stroke.line(to: NSPoint(x: inset.minX + x1 * sx, y: inset.minY + y1 * sy))
            }
            NSColor.white.setStroke()
            stroke.stroke()
            return true
        }
        image.isTemplate = false
        image.accessibilityDescription = dot.help
        return image
    }
}
