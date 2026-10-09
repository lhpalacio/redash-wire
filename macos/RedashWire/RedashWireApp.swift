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
        .commands {
            // The app menu exists while a window gives the app a Dock icon.
            CommandGroup(replacing: .appSettings) {
                Button("Settings…") { WindowPresenter.shared.show("settings") }
                    .keyboardShortcut(",")
            }
        }
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
        WindowPresenter.shared.content = { [model] id in
            switch id {
            case "logs":
                return WindowSpec(title: "redash-wire Logs", resizable: true, size: NSSize(width: 760, height: 440),
                                  view: LogWindow(log: model.supervisor.log, diagnostics: model.diagnostics))
            case "onboarding":
                return WindowSpec(title: "Set up redash-wire", view: OnboardingView(model: model))
            case "settings":
                return WindowSpec(title: "redash-wire Settings",
                                  view: SettingsView(model: model, supervisor: model.supervisor, updates: model.updates, notifier: model.notifier))
            default:
                return nil
            }
        }
        WindowPresenter.shared.followWindows()
        Task {
            await model.start()
            // Only for a missing file. A config that will not parse, or a
            // binary that will not run, is an error the menu shows; the
            // wizard would only answer "a config already exists".
            if model.needsOnboarding {
                WindowPresenter.shared.show("onboarding")
            }
            #if DEBUG
            presentForScreenshot()
            #endif
        }
    }

    #if DEBUG
    /// `-present <window id>` or `-present menu`, for dev/screenshots.sh.
    private func presentForScreenshot() {
        guard let target = UserDefaults.standard.string(forKey: "present") else { return }
        DispatchQueue.main.asyncAfter(deadline: .now() + 3) {
            guard target == "menu" else {
                WindowPresenter.shared.show(target)
                return
            }
            func button(in view: NSView?) -> NSStatusBarButton? {
                guard let view else { return nil }
                if let button = view as? NSStatusBarButton { return button }
                return view.subviews.lazy.compactMap { button(in: $0) }.first
            }
            NSApp.windows.lazy.compactMap { button(in: $0.contentView) }.first?.performClick(nil)
        }
    }
    #endif

    /// Opening the app again from Finder or Spotlight shows something, rather
    /// than nothing at all when the menu bar has no room for the icon.
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if !flag {
            WindowPresenter.shared.show(model.needsOnboarding ? "onboarding" : "settings")
        }
        return false
    }

    func applicationWillTerminate(_ notification: Notification) {
        Clipboard.clearSecretOnQuit()
        model.notifier.cancelPending()
    }
}

struct WindowSpec {
    let title: String
    var resizable = false
    /// The first size, before the window has a saved frame. Nil fits the content.
    var size: NSSize?
    let view: AnyView

    init<V: View>(title: String, resizable: Bool = false, size: NSSize? = nil, view: V) {
        self.title = title
        self.resizable = resizable
        self.size = size
        self.view = AnyView(view)
    }
}

/// Opens the app's windows in front, and gives the app a Dock icon while one is
/// open so it can be found again with ⌘-Tab. The windows are AppKit's, not
/// SwiftUI scenes: a scene opens only through a view's `openWindow`, and with
/// the status item hidden no view ever appears to hand it over.
@MainActor
final class WindowPresenter {
    static let shared = WindowPresenter()

    var content: ((String) -> WindowSpec?)?
    private var windows: [String: NSWindow] = [:]

    func show(_ id: String) {
        guard let window = windows[id] ?? makeWindow(id) else { return }
        NSApp.setActivationPolicy(.regular)
        Self.activate()
        window.makeKeyAndOrderFront(nil)
    }

    func close(_ id: String) {
        windows[id]?.close()
    }

    /// The autosave name is the id, which keeps the frames the SwiftUI scenes
    /// saved under the same names.
    private func makeWindow(_ id: String) -> NSWindow? {
        guard let spec = content?(id) else { return nil }
        let controller = NSHostingController(rootView: spec.view)
        controller.sizingOptions = spec.resizable ? [.minSize] : [.preferredContentSize]
        let window = NSWindow(contentViewController: controller)
        window.title = spec.title
        window.styleMask = [.titled, .closable, .miniaturizable]
        if spec.resizable {
            window.styleMask.insert(.resizable)
        }
        window.isReleasedWhenClosed = false
        if !window.setFrameUsingName(id) {
            if let size = spec.size {
                window.setContentSize(size)
            }
            window.center()
        }
        window.setFrameAutosaveName(id)
        windows[id] = window
        return window
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

    var body: some View {
        let summary = supervisor.statusSummary()
        Image(systemName: Self.symbolName(for: summary.tone, state: supervisor.state))
            .accessibilityLabel("redash-wire: \(summary.headline)")
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
