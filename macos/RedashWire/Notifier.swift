import AppKit
import Combine
import UserNotifications

/// Posts `StatusAlert`s as notifications, worded like the menu. The menu bar
/// icon used to be the only sign that Redash had gone or a start had given up.
@MainActor
final class Notifier: NSObject, ObservableObject {
    @Published var isEnabled: Bool {
        didSet {
            UserDefaults.standard.set(isEnabled, forKey: Self.enabledKey)
            if isEnabled {
                requestAuthorization()
            } else {
                center.removeAllPendingNotificationRequests()
            }
        }
    }

    /// Turned off for this app in System Settings, which the toggle cannot override.
    @Published private(set) var isDenied = false

    private static let enabledKey = "notificationsEnabled"
    private nonisolated static let offlineID = "offline"

    private let supervisor: ProxySupervisor
    private let center = UNUserNotificationCenter.current()
    private var last: StatusSummary
    private var subscription: AnyCancellable?

    init(supervisor: ProxySupervisor) {
        self.supervisor = supervisor
        self.isEnabled = UserDefaults.standard.object(forKey: Self.enabledKey) as? Bool ?? true
        self.last = supervisor.statusSummary()
        super.init()
        center.delegate = self

        // @Published fires before the value changes, so the summary is read on
        // the next turn of the run loop, once it has.
        subscription = supervisor.$snapshot
            .receive(on: DispatchQueue.main)
            .sink { [weak self] _ in self?.statusChanged() }
    }

    func requestAuthorization() {
        guard isEnabled else { return }
        center.requestAuthorization(options: [.alert]) { [weak self] granted, _ in
            Task { @MainActor in self?.isDenied = !granted }
        }
    }

    func openSystemSettings() {
        let id = Bundle.main.bundleIdentifier ?? ""
        if let url = URL(string: "x-apple.systempreferences:com.apple.Notifications-Settings.extension?id=\(id)") {
            NSWorkspace.shared.open(url)
        }
    }

    private func statusChanged() {
        let current = supervisor.statusSummary()
        defer { last = current }
        guard isEnabled else { return }

        for alert in StatusAlert.changes(from: last, to: current) {
            switch alert {
            case .offline:
                post(current, id: Self.offlineID, after: StatusAlert.offlineDelay)
            case .backOnline:
                center.removePendingNotificationRequests(withIdentifiers: [Self.offlineID])
                center.getDeliveredNotifications { [weak self] delivered in
                    guard delivered.contains(where: { $0.request.identifier == Self.offlineID }) else { return }
                    Task { @MainActor in
                        guard let self else { return }
                        self.center.removeDeliveredNotifications(withIdentifiers: [Self.offlineID])
                        self.post(StatusSummary(tone: .ok, headline: "Redash is back", details: ["The proxy is serving again."]), id: "online")
                    }
                }
            case .cancelOffline:
                center.removePendingNotificationRequests(withIdentifiers: [Self.offlineID])
            case .needsAttention:
                post(current, id: "attention")
            }
        }
    }

    private func post(_ summary: StatusSummary, id: String, after delay: TimeInterval? = nil) {
        let content = UNMutableNotificationContent()
        content.title = summary.headline
        if let profile = supervisor.activeProfile?.name {
            content.subtitle = profile
        }
        content.body = summary.details.joined(separator: " ")
        let trigger = delay.map { UNTimeIntervalNotificationTrigger(timeInterval: $0, repeats: false) }
        center.add(UNNotificationRequest(identifier: id, content: content, trigger: trigger))
    }
}

extension Notifier: UNUserNotificationCenterDelegate {
    /// A menu bar app counts as active while its menu is open, which would
    /// otherwise swallow the banner.
    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        willPresent notification: UNNotification,
        withCompletionHandler completionHandler: @escaping (UNNotificationPresentationOptions) -> Void
    ) {
        completionHandler([.banner])
    }

    nonisolated func userNotificationCenter(
        _ center: UNUserNotificationCenter,
        didReceive response: UNNotificationResponse,
        withCompletionHandler completionHandler: @escaping () -> Void
    ) {
        Task { @MainActor in
            WindowPresenter.shared.show("logs")
            completionHandler()
        }
    }
}
