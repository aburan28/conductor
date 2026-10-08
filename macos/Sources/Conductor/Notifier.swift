import AppKit
import Foundation
import UserNotifications

/// Notifications: a session hit its usage limit (with "Open Checkpoints"), a join finished.
final class Notifier: NSObject, UNUserNotificationCenterDelegate {
    static let shared = Notifier()

    static let usageLimitCategory = "dev.conductor.usage-limit"
    static let openCheckpointsAction = "dev.conductor.open-checkpoints"

    private var center: UNUserNotificationCenter { UNUserNotificationCenter.current() }

    func setUp() {
        center.delegate = self
        let open = UNNotificationAction(identifier: Self.openCheckpointsAction, title: "Open Checkpoints", options: [.foreground])
        let category = UNNotificationCategory(identifier: Self.usageLimitCategory, actions: [open], intentIdentifiers: [], options: [])
        center.setNotificationCategories([category])
        center.requestAuthorization(options: [.alert, .sound]) { _, _ in }
    }

    func usageLimit(title: String, body: String) {
        post(title: title, body: body, category: Self.usageLimitCategory)
    }

    func post(title: String, body: String, category: String? = nil) {
        let content = UNMutableNotificationContent()
        content.title = title
        content.body = body
        content.sound = .default
        if let category { content.categoryIdentifier = category }
        center.add(UNNotificationRequest(identifier: UUID().uuidString, content: content, trigger: nil))
    }

    // The async forms of the delegate methods: their completion-handler forms changed
    // annotations between SDKs, and a witness that stops matching is silently never called.

    /// Shown even while the app is in front: a limit is worth interrupting for.
    func userNotificationCenter(_ center: UNUserNotificationCenter,
                                willPresent notification: UNNotification) async -> UNNotificationPresentationOptions {
        [.banner, .sound]
    }

    func userNotificationCenter(_ center: UNUserNotificationCenter, didReceive response: UNNotificationResponse) async {
        guard response.notification.request.content.categoryIdentifier == Self.usageLimitCategory else { return }
        await MainActor.run {
            NSApp.activate(ignoringOtherApps: true)
            AppModel.shared.requestWindow("checkpoints")
        }
    }
}
