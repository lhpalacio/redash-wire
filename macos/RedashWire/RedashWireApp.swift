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

    private var symbolName: String {
        switch model.supervisor.state {
        case .running:
            return "bolt.horizontal.circle.fill"
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
        MenuBarView(model: model, supervisor: model.supervisor) { id in
            // A menu bar app is not active, so the window would open behind others.
            NSApplication.shared.activate(ignoringOtherApps: true)
            openWindow(id: id)
        }
    }
}
