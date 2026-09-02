import SwiftUI

@main
struct RedashWireApp: App {
    @StateObject private var model = AppModel()

    init() {
        // Writing the API key to a child that has already died — an exec
        // failure, quarantine, the wrong architecture — raises SIGPIPE, whose
        // default action is to kill the whole app with no error anywhere.
        // Ignored, the write fails with EPIPE, which WireCLI reports.
        signal(SIGPIPE, SIG_IGN)
    }

    var body: some Scene {
        MenuBarExtra {
            MenuRoot(model: model)
        } label: {
            MenuBarLabel(model: model, supervisor: model.supervisor)
        }
        .menuBarExtraStyle(.menu)

        Window("redash-wire Logs", id: "logs") {
            LogWindow(log: model.supervisor.log)
        }
        .defaultSize(width: 760, height: 440)

        Window("Set up redash-wire", id: "onboarding") {
            OnboardingView(model: model)
        }
        .windowResizability(.contentSize)
    }
}

/// Rendered at launch, unlike the menu contents, so loading starts here.
///
/// Observes the supervisor itself. Reading its state through the model is not
/// enough: a nested ObservableObject does not publish through its parent, so the
/// icon would keep the state it had at launch.
private struct MenuBarLabel: View {
    @ObservedObject var model: AppModel
    @ObservedObject var supervisor: ProxySupervisor
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Image(systemName: symbolName)
            .task {
                await model.start()
                // Only for a missing file. A config that will not parse, or a
                // binary that will not run, is an error the menu shows; the
                // wizard would only answer "a config already exists".
                if model.needsOnboarding {
                    NSApplication.shared.activate(ignoringOtherApps: true)
                    openWindow(id: "onboarding")
                }
            }
    }

    /// The menu bar renders these as template images, so colour cannot carry the
    /// difference — the symbol has to. A proxy that is up but cut off from Redash
    /// gets its own, because leaving it looking healthy is the thing that made a
    /// disconnected VPN invisible until a query failed.
    private var symbolName: String {
        switch supervisor.state {
        case .running(_, .ok):
            return "bolt.horizontal.circle.fill"
        case .running(_, .unreachable):
            return "bolt.slash.circle.fill"
        case .running(_, .rejected):
            return "exclamationmark.triangle.fill"
        case .starting, .running(_, .checking):
            return "bolt.horizontal.circle"
        case .failed:
            return "exclamationmark.triangle.fill"
        case .stopped:
            return "bolt.horizontal.circle"
        }
    }
}

/// `openWindow` is only available inside a View.
private struct MenuRoot: View {
    @ObservedObject var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        MenuBarView(model: model, supervisor: model.supervisor, updates: model.updates) { id in
            // A menu bar app is not active, so the window would open behind others.
            NSApplication.shared.activate(ignoringOtherApps: true)
            openWindow(id: id)
        }
    }
}
