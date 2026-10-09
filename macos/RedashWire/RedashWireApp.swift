import SwiftUI

@main
struct RedashWireApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        MenuBarExtra {
            MenuRoot(model: delegate.model)
        } label: {
            MenuBarLabel(model: delegate.model, supervisor: delegate.model.supervisor)
        }
        .menuBarExtraStyle(.menu)

        Window("redash-wire Logs", id: "logs") {
            LogWindow(log: delegate.model.supervisor.log)
        }
        .defaultSize(width: 760, height: 440)

        Window("Set up redash-wire", id: "onboarding") {
            OnboardingView(model: delegate.model)
        }
        .windowResizability(.contentSize)
    }
}

/// Owns the model so launch does not depend on a view: the status item can be
/// hidden in System Settings › Menu Bar, and a label that never renders used to
/// mean a proxy that never started.
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let model = AppModel()

    override init() {
        // Writing the API key to a child that has already died — an exec
        // failure, quarantine, the wrong architecture — raises SIGPIPE, whose
        // default action is to kill the whole app with no error anywhere.
        // Ignored, the write fails with EPIPE, which WireCLI reports.
        signal(SIGPIPE, SIG_IGN)
        super.init()
    }

    func applicationDidFinishLaunching(_ notification: Notification) {
        WindowPresenter.shared.followWindows()
        Task {
            await model.start()
            // Only for a missing file. A config that will not parse, or a
            // binary that will not run, is an error the menu shows; the
            // wizard would only answer "a config already exists".
            if model.needsOnboarding {
                WindowPresenter.shared.show("onboarding")
            }
        }
    }

    /// Opening the app again from Finder or Spotlight shows something, rather
    /// than nothing at all when the menu bar has no room for the icon.
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag {
            WindowPresenter.shared.show(model.needsOnboarding ? "onboarding" : "logs")
        }
        return false
    }

    func applicationWillTerminate(_ notification: Notification) {
        Clipboard.clearSecretOnQuit()
    }
}

/// Opens the app's windows in front, and gives the app a Dock icon while one is
/// open so it can be found again with ⌘-Tab. `openWindow` only exists inside a
/// view, so the menu bar label hands it over when it appears; a request made
/// before that waits for it.
@MainActor
final class WindowPresenter {
    static let shared = WindowPresenter()

    private var open: ((String) -> Void)?
    private var pending: String?

    func register(_ open: @escaping (String) -> Void) {
        self.open = open
        if let pending {
            self.pending = nil
            show(pending)
        }
    }

    func show(_ id: String) {
        guard let open else {
            pending = id
            return
        }
        NSApp.setActivationPolicy(.regular)
        Self.activate()
        open(id)
    }

    /// A menu bar app is not active, so anything it shows opens behind others.
    static func activate() {
        if #available(macOS 14.0, *) {
            NSApp.activate()
        } else {
            NSApp.activate(ignoringOtherApps: true)
        }
    }

    /// Back to a menu bar app once the last window closes.
    func followWindows() {
        NotificationCenter.default.addObserver(forName: NSWindow.willCloseNotification, object: nil, queue: .main) { note in
            let closing = note.object as? NSWindow
            DispatchQueue.main.async {
                let stillOpen = NSApp.windows.contains { $0 !== closing && $0.isVisible && $0.styleMask.contains(.titled) }
                if !stillOpen {
                    NSApp.setActivationPolicy(.accessory)
                }
            }
        }
    }
}

/// Observes the supervisor itself. Reading its state through the model is not
/// enough: a nested ObservableObject does not publish through its parent, so the
/// icon would keep the state it had at launch.
private struct MenuBarLabel: View {
    @ObservedObject var model: AppModel
    @ObservedObject var supervisor: ProxySupervisor
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        let summary = supervisor.statusSummary()
        Image(systemName: Self.symbolName(for: summary.tone, state: supervisor.state))
            .accessibilityLabel("redash-wire: \(summary.headline)")
            .onAppear {
                let openWindow = openWindow
                WindowPresenter.shared.register { openWindow(id: $0) }
            }
    }

    /// The menu bar renders these as template images, so colour cannot carry the
    /// difference — the symbol has to. A proxy that is up but cut off from Redash
    /// gets its own, because leaving it looking healthy is the thing that made a
    /// disconnected VPN invisible until a query failed.
    private static func symbolName(for tone: StatusSummary.Tone, state: ProxySupervisor.State) -> String {
        switch tone {
        case .ok:
            return "bolt.horizontal.circle.fill"
        case .idle:
            return "bolt.horizontal.circle"
        case .busy:
            return "arrow.triangle.2.circlepath.circle"
        case .warning:
            return "bolt.slash.circle.fill"
        case .error:
            if case .gaveUp(.unreachable) = state {
                return "bolt.slash.circle"
            }
            return "exclamationmark.triangle.fill"
        }
    }
}

private struct MenuRoot: View {
    @ObservedObject var model: AppModel

    var body: some View {
        MenuBarView(model: model, supervisor: model.supervisor, updates: model.updates) { id in
            WindowPresenter.shared.show(id)
        }
    }
}
