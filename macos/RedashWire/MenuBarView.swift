import SwiftUI

/// The `.menu` style renders a real NSMenu, so this is limited to Text, Button,
/// Toggle, Divider and nested Menu.
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
            Button("Quit redash-wire") { NSApplication.shared.terminate(nil) }
                .keyboardShortcut("q")
        }
    }


    @ViewBuilder
    private var statusSection: some View {
        Text(statusLine)

        if case .failed(let reason) = supervisor.state {
            Text(reason)
            Button("Show logs") { openWindow("logs") }
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
        Button(supervisor.state.isRunning || supervisor.state.isBusy ? "Stop" : "Start") {
            Task { await model.toggleProxy() }
        }
        .disabled(model.selectedProfile == nil)

        if case .failed = supervisor.state {
            Button("Retry") {
                Task {
                    if let profile = model.selectedProfile {
                        supervisor.start(profile: profile)
                    }
                }
            }
        }
    }


    @ViewBuilder
    private var profileSection: some View {
        if model.profiles.count > 1 || model.isConfigured {
            Menu("Profile") {
                ForEach(model.profiles) { profile in
                    Toggle(profileLabel(profile), isOn: Binding(
                        get: { profile.name == model.selectedProfileName },
                        set: { isOn in
                            guard isOn else { return }
                            Task { await model.select(profile: profile) }
                        }
                    ))
                }
            }
        }
    }

    private func profileLabel(_ profile: Profile) -> String {
        profile.valid ? profile.name : "\(profile.name) (invalid)"
    }


    @ViewBuilder
    private var dataSourceSection: some View {
        Menu(dataSourceMenuTitle) {
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
            Menu("Connect") {
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
            }
        }
    }


    @ViewBuilder
    private var utilitySection: some View {
        Button("Show logs") { openWindow("logs") }

        Toggle("Launch at login", isOn: Binding(
            get: { model.launchesAtLogin },
            set: { enabled in try? model.setLaunchAtLogin(enabled) }
        ))

        Button("Edit config…") { model.openConfigInEditor() }
        Button("Reveal config in Finder") { model.revealConfigInFinder() }
        Button("Reload config") { Task { await model.reloadConfig() } }

        if !model.isConfigured {
            Button("Set up redash-wire…") { openWindow("onboarding") }
        }
    }
}
