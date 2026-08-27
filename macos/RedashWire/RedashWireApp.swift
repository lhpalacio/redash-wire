import SwiftUI

@main
struct RedashWireApp: App {
    @StateObject private var model = AppModel()

    var body: some Scene {
        MenuBarExtra {
            MenuRoot(model: model)
        } label: {
            MenuBarLabel(model: model)
        }
        .menuBarExtraStyle(.menu)

        Window("redash-wire Logs", id: "logs") {
            LogWindow(supervisor: model.supervisor)
        }
        .defaultSize(width: 760, height: 440)

        Window("Set up redash-wire", id: "onboarding") {
            OnboardingView(model: model)
        }
        .windowResizability(.contentSize)
    }
}

/// Rendered at launch, unlike the menu contents, so loading starts here.
private struct MenuBarLabel: View {
    @ObservedObject var model: AppModel
    @Environment(\.openWindow) private var openWindow

    var body: some View {
        Image(systemName: symbolName)
            .task {
                await model.start()
                if !model.isConfigured {
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
        switch model.supervisor.state {
        case .running(_, .ok):
            return "bolt.horizontal.circle.fill"
        case .running(_, .unreachable):
            return "bolt.slash.circle.fill"
        case .running(_, .rejected):
            return "exclamationmark.triangle.fill"
        case .starting:
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
