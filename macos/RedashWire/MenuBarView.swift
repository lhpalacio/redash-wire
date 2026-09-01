import AppKit
import SwiftUI

extension String {
    /// An NSMenu item is a single line that never wraps: the menu widens to fit
    /// the longest one, so anything that came from an error or from the config
    /// has to be bounded before it gets here.
    func fittedToMenu(limit: Int = 64) -> String {
        guard count > limit else { return self }
        return prefix(limit - 1).trimmingCharacters(in: .whitespaces) + "…"
    }
}

/// The `.menu` style renders a real NSMenu, so this is limited to Text, Button,
/// Toggle, Divider and nested Menu.
///
/// Icons stop at the root menu. Profile and Data sources are lists of like things,
/// where an icon column repeats the same symbol down every row.
struct MenuBarView: View {
    @ObservedObject var model: AppModel
    @ObservedObject var supervisor: ProxySupervisor
    @ObservedObject var updates: UpdateChecker
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
            Text(reason.fittedToMenu())
            showLogsButton
        } else if let health = supervisor.state.health, !health.isOK {
            // The listeners are bound but nothing behind them can be served, so
            // the addresses would be a lie. Show the cause and the fix instead.
            if let summary = health.summary {
                Text(summary)
            }
            if let remedy = health.remedy {
                Text(remedy)
            }
            if let line = retryLine {
                Text(line)
            }
            showLogsButton
        } else if supervisor.state.isRunning, let profile = model.selectedProfile {
            ForEach(listenerLines(for: profile), id: \.self) { line in
                Text(line)
            }
        } else if supervisor.state.isBusy, let restart = supervisor.pendingRestart {
            Text("Restarting in \(Self.countdown(to: restart.at, now: supervisor.now)) (attempt \(restart.attempt) of \(restart.limit))")
        }

        if let error = model.configError {
            Text(error.message.fittedToMenu())
            if let remedy = error.remedy {
                Text(remedy)
            }
        }
    }

    @ViewBuilder
    private var showLogsButton: some View {
        Button {
            openWindow("logs")
        } label: {
            Label("Show Logs", systemImage: "list.bullet.rectangle")
        }
    }

    private var statusLine: String {
        let name = (model.selectedProfileName ?? "no profile").fittedToMenu(limit: 24)
        switch supervisor.state {
        case .stopped:
            return "Stopped — \(name)"
        case .starting:
            return "Starting — \(name)"
        case .running(let since, .ok):
            return "Running — \(name) (\(Self.uptime(since: since)))"
        case .running(_, .unreachable):
            return "Redash unreachable — \(name)"
        case .running(_, .rejected):
            return "Redash rejected the API key — \(name)"
        case .failed:
            return "Failed — \(name)"
        }
    }

    /// Stopped is grey, not red: you stopped it on purpose. Amber separates a
    /// Redash that should come back on its own from a red one that needs you to
    /// go and change something.
    private static func statusColor(for state: ProxySupervisor.State) -> NSColor {
        switch state {
        case .stopped: return .systemGray
        case .starting: return .systemYellow
        case .running(_, .ok): return .systemGreen
        case .running(_, .unreachable): return .systemOrange
        case .running(_, .rejected): return .systemRed
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

    /// Reads `now` so the row re-renders on every tick. Once the count reaches
    /// zero the probe is in flight for up to its timeout, which is not a number.
    private var retryLine: String? {
        guard let at = supervisor.nextProbeAt else { return nil }
        let remaining = at.timeIntervalSince(supervisor.now)
        return remaining > 0.5 ? "Retrying in \(Self.countdown(to: at, now: supervisor.now))" : "Retrying now…"
    }

    private static func countdown(to date: Date, now: Date) -> String {
        let seconds = max(0, Int(date.timeIntervalSince(now).rounded(.up)))
        if seconds < 60 { return "\(seconds)s" }
        return "\(seconds / 60)m \(seconds % 60)s"
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
            if model.dataSources.isEmpty {
                Text(emptyDataSourceMessage)
            } else {
                // One section per wire protocol, so the list reads as "these
                // are Postgres, these are MySQL" rather than one run of names.
                ForEach(Array(model.dataSourceGroups.enumerated()), id: \.element.id) { index, group in
                    if index > 0 {
                        Divider()
                    }
                    Text(group.title)
                    ForEach(group.sources) { source in
                        Menu(source.name) {
                            copyActions(for: source)
                        }
                    }
                }

                if !model.unservableDataSources.isEmpty {
                    Divider()
                    Text("Not served by the proxy")
                    ForEach(model.unservableDataSources) { source in
                        Text("\(source.name) (\(source.type))")
                    }
                }
            }
        } label: {
            Label(dataSourceMenuTitle, systemImage: "cylinder.split.1x2")
        }
    }

    /// The list is empty for four different reasons and they are worth telling
    /// apart, since only the last one is about Redash having nothing to serve.
    private var emptyDataSourceMessage: String {
        if supervisor.state.isBusy {
            return "Loading…"
        }
        if !supervisor.state.isRunning {
            return "Start the proxy to list data sources"
        }
        if let health = supervisor.state.health, !health.isOK {
            return "Waiting for Redash"
        }
        return "No data sources"
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
        // Added by the daily background check. Nothing is downloaded and nothing
        // interrupts you: the row is the whole notification.
        if let release = updates.available {
            Button {
                updates.openReleasePage()
            } label: {
                Label("Update available — \(release.version)", systemImage: "arrow.down.circle")
            }
        }

        Button {
            Task { await updates.checkNow() }
        } label: {
            Label("Check for Updates…", systemImage: "arrow.triangle.2.circlepath")
        }
        .disabled(updates.isChecking)

        Button {
            NSApplication.shared.activate(ignoringOtherApps: true)
            NSApplication.shared.orderFrontStandardAboutPanel(nil)
        } label: {
            Label("About redash-wire", systemImage: "info.circle")
        }
    }
}
