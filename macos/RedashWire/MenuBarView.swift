import AppKit
import SwiftUI

/// The `.menu` style renders a real NSMenu, so this is limited to Text, Button,
/// Toggle, Divider and nested Menu.
///
/// Icons stop at the root menu. Profile and Data sources are lists of like things,
/// where an icon column repeats the same symbol down every row.
struct MenuBarView: View {
    @ObservedObject var model: AppModel
    @ObservedObject var supervisor: ProxySupervisor
    let openWindow: (String) -> Void

    var body: some View {
        Group {
            statusSection
            Divider()
            controlSection
            Divider()
            profileSection
            dataSourceSection
            connectSection
            Divider()
            utilitySection
            Divider()
            aboutSection
            Button {
                NSApplication.shared.terminate(nil)
            } label: {
                Label("Quit redash-wire", systemImage: "xmark.circle")
            }
            .keyboardShortcut("q")
        }
    }


    @ViewBuilder
    private var statusSection: some View {
        Label {
            Text(statusLine)
        } icon: {
            Image(nsImage: Self.dot(Self.statusColor(for: supervisor.state)))
                .renderingMode(.original)
        }

        if case .failed(let reason) = supervisor.state {
            Text(reason)
            Button {
                openWindow("logs")
            } label: {
                Label("Show Logs", systemImage: "list.bullet.rectangle")
            }
        } else if supervisor.state.isRunning, let profile = model.selectedProfile {
            ForEach(listenerLines(for: profile), id: \.self) { line in
                Text(line)
            }
        }

        if let error = model.configError {
            Text(error.message)
            if let remedy = error.remedy {
                Text(remedy)
            }
        }
    }

    private var statusLine: String {
        let name = model.selectedProfileName ?? "no profile"
        switch supervisor.state {
        case .stopped:
            return "Stopped — \(name)"
        case .starting:
            return "Starting — \(name)"
        case .running(let since):
            return "Running — \(name) (\(Self.uptime(since: since)))"
        case .failed:
            return "Failed — \(name)"
        }
    }

    /// Stopped is grey, not red: you stopped it on purpose. Red is left for a crash.
    private static func statusColor(for state: ProxySupervisor.State) -> NSColor {
        switch state {
        case .stopped: return .systemGray
        case .starting: return .systemYellow
        case .running: return .systemGreen
        case .failed: return .systemRed
        }
    }

    /// Drawn instead of an SF Symbol: SwiftUI hands symbols to NSMenuItem as template
    /// images, which AppKit repaints in the system tint. `isTemplate = false` opts out
    /// on the AppKit side, `.renderingMode(.original)` at the call site opts out on
    /// the SwiftUI side. Both, because the bridge between them is undocumented.
    private static func dot(_ color: NSColor) -> NSImage {
        let image = NSImage(size: NSSize(width: 10, height: 10), flipped: false) { rect in
            color.setFill()
            NSBezierPath(ovalIn: rect.insetBy(dx: 1, dy: 1)).fill()
            return true
        }
        image.isTemplate = false
        return image
    }

    private func listenerLines(for profile: Profile) -> [String] {
        var lines: [String] = []
        if !profile.postgresListenAddr.isEmpty {
            lines.append("PostgreSQL  \(profile.postgresListenAddr)")
        }
        if !profile.mysqlListenAddr.isEmpty {
            lines.append("MySQL  \(profile.mysqlListenAddr)")
        }
        return lines
    }

    private static func uptime(since: Date) -> String {
        let seconds = Int(Date().timeIntervalSince(since))
        if seconds < 60 { return "\(seconds)s" }
        if seconds < 3600 { return "\(seconds / 60)m" }
        return "\(seconds / 3600)h \((seconds % 3600) / 60)m"
    }


    @ViewBuilder
    private var controlSection: some View {
        let stopping = supervisor.state.isRunning || supervisor.state.isBusy

        Button {
            Task { await model.toggleProxy() }
        } label: {
            Label(stopping ? "Stop" : "Start", systemImage: stopping ? "stop.fill" : "play.fill")
        }
        .disabled(model.selectedProfile == nil)

        if case .failed = supervisor.state {
            Button {
                Task {
                    if let profile = model.selectedProfile {
                        supervisor.start(profile: profile)
                    }
                }
            } label: {
                Label("Retry", systemImage: "arrow.clockwise")
            }
        }
    }


    @ViewBuilder
    private var profileSection: some View {
        if model.profiles.count > 1 || model.isConfigured {
            Menu {
                ForEach(model.profiles) { profile in
                    Toggle(profileLabel(profile), isOn: Binding(
                        get: { profile.name == model.selectedProfileName },
                        set: { isOn in
                            guard isOn else { return }
                            Task { await model.select(profile: profile) }
                        }
                    ))
                }
            } label: {
                Label("Profile", systemImage: "rectangle.stack")
            }
        }
    }

    private func profileLabel(_ profile: Profile) -> String {
        profile.valid ? profile.name : "\(profile.name) (invalid)"
    }


    @ViewBuilder
    private var dataSourceSection: some View {
        Menu {
            if let error = model.dataSourcesError {
                Text(error.message)
                if let remedy = error.remedy {
                    Text(remedy)
                }
                Button("Retry") { Task { await model.refreshDataSources() } }
            } else if model.isLoadingDataSources {
                Text("Loading…")
            } else if model.dataSources.isEmpty {
                Text("No data sources")
                Button("Refresh") { Task { await model.refreshDataSources() } }
            } else {
                ForEach(model.servableDataSources) { source in
                    Menu(source.name) {
                        copyActions(for: source)
                    }
                }

                if !model.unservableDataSources.isEmpty {
                    Divider()
                    Text("Not served by the proxy")
                    ForEach(model.unservableDataSources) { source in
                        Text("\(source.name) (\(source.type))")
                    }
                }

                Divider()
                Button("Refresh") { Task { await model.refreshDataSources() } }
            }
        } label: {
            Label(dataSourceMenuTitle, systemImage: "cylinder.split.1x2")
        }
    }

    private var dataSourceMenuTitle: String {
        model.dataSources.isEmpty ? "Data sources" : "Data sources (\(model.servableDataSources.count))"
    }

    @ViewBuilder
    private func copyActions(for source: DataSource) -> some View {
        if let profile = model.selectedProfile {
            if source.wire == "postgres" {
                if let command = ConnectionStrings.psql(profile: profile, database: source.name) {
                    Button("Copy psql command") { Clipboard.copySecret(command) }
                }
                if let uri = ConnectionStrings.postgresURI(profile: profile, database: source.name) {
                    Button("Copy connection URI") { Clipboard.copySecret(uri) }
                }
            } else if source.wire == "mysql" {
                if let command = ConnectionStrings.mysql(profile: profile, database: source.name) {
                    Button("Copy mysql command") { Clipboard.copySecret(command) }
                }
                if let uri = ConnectionStrings.mysqlURI(profile: profile, database: source.name) {
                    Button("Copy connection URI") { Clipboard.copySecret(uri) }
                }
            }
            Button("Copy database name") { Clipboard.copy(source.name) }
        }
    }


    @ViewBuilder
    private var connectSection: some View {
        if let profile = model.selectedProfile {
            Menu {
                if let command = ConnectionStrings.psql(profile: profile, database: nil) {
                    Button("Copy psql command") { Clipboard.copySecret(command) }
                }
                if let command = ConnectionStrings.mysql(profile: profile, database: nil) {
                    Button("Copy mysql command") { Clipboard.copySecret(command) }
                }
                Divider()
                Button("Copy username") { Clipboard.copy(profile.username) }
                Button("Copy password") { Clipboard.copySecret(profile.password) }
                if profile.defaultCredentials {
                    Text("Using the built-in default credentials")
                }
            } label: {
                Label("Connect", systemImage: "link")
            }
        }
    }


    @ViewBuilder
    private var utilitySection: some View {
        Button {
            openWindow("logs")
        } label: {
            Label("Show Logs", systemImage: "list.bullet.rectangle")
        }

        Toggle(isOn: Binding(
            get: { model.launchesAtLogin },
            set: { enabled in try? model.setLaunchAtLogin(enabled) }
        )) {
            Label("Launch at Login", systemImage: "power")
        }

        // The shortcuts only fire while the menu is open: LSUIElement leaves no app
        // menu to register a key equivalent with.
        Button {
            // Config deleted out from under us, so there is nothing to edit yet.
            if !model.openConfigInEditor() {
                openWindow("onboarding")
            }
        } label: {
            Label("Settings…", systemImage: "gearshape")
        }
        .keyboardShortcut(",")

        Button {
            Task { await model.reloadConfig(applyToRunning: true) }
        } label: {
            Label("Reload Configuration", systemImage: "arrow.clockwise")
        }
        .keyboardShortcut(",", modifiers: [.command, .shift])

        if !model.isConfigured {
            Button {
                openWindow("onboarding")
            } label: {
                Label("Set up redash-wire…", systemImage: "wand.and.stars")
            }
        }
    }


    /// LSUIElement leaves no app menu, so this is the only place the version shows.
    @ViewBuilder
    private var aboutSection: some View {
        Button {
            NSApplication.shared.activate(ignoringOtherApps: true)
            NSApplication.shared.orderFrontStandardAboutPanel(nil)
        } label: {
            Label("About redash-wire", systemImage: "info.circle")
        }
    }
}
