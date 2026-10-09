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
        let summary = supervisor.statusSummary()
        Label {
            Text(headline(for: summary))
        } icon: {
            Image(nsImage: Self.dot(Self.color(for: summary.tone)))
                .renderingMode(.original)
        }
        ForEach(summary.details, id: \.self) { line in
            Text(line)
        }
        ForEach(summary.actions, id: \.self) { action in
            button(for: action)
        }

        if let hint = model.runningProfileDrift.flatMap(driftHint) {
            Text(hint.fittedToMenu(limit: 80))
        }

        if supervisor.state == .stopped, let notice = model.reloadNotice {
            Text(notice.fittedToMenu(limit: 80))
        }

        if let error = model.configError {
            Text(error.message.fittedToMenu())
            if !summary.actions.contains(.editConfiguration) {
                button(for: .editConfiguration)
            }
        }
    }

    @ViewBuilder
    private func button(for action: StatusSummary.Action) -> some View {
        switch action {
        case .retry:
            Button {
                Task { await model.retry() }
            } label: {
                Label("Retry", systemImage: "arrow.clockwise")
            }
        case .checkNow:
            Button {
                supervisor.checkNow()
            } label: {
                Label("Check Now", systemImage: "arrow.clockwise")
            }
        case .editConfiguration:
            Button {
                openSettings()
            } label: {
                Label("Edit Configuration…", systemImage: "doc.text")
            }
        case .showLogs:
            showLogsButton
        }
    }

    /// The proxy reads its profile once, at launch. Saving the file changes
    /// nothing until Reload Configuration, and the menu has to say so, or the
    /// listener lines above it look wrong against the file.
    private func driftHint(_ drift: ProfileDrift) -> String? {
        switch drift {
        case .unchanged:
            return nil
        case .edited:
            return "Config changed — Reload Configuration to apply"
        case .removed:
            let running = supervisor.activeProfile?.name ?? "?"
            if let next = model.selectedProfileName {
                return "Profile “\(running)” is gone from the config — reload to switch to “\(next)”"
            }
            return "Profile “\(running)” is gone from the config — reload to stop"
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

    private func headline(for summary: StatusSummary) -> String {
        guard let name = model.describedProfileName else { return summary.headline }
        var line = "\(summary.headline) · \(name.fittedToMenu(limit: 24))"
        if model.describedReadOnly {
            line += " · read-only"
        }
        return line
    }

    /// Stopped is grey, not red: you stopped it on purpose. Amber separates a
    /// Redash that should come back on its own from a red one that needs you to
    /// go and change something.
    private static func color(for tone: StatusSummary.Tone) -> NSColor {
        switch tone {
        case .idle: return .systemGray
        case .busy: return .systemYellow
        case .ok: return .systemGreen
        case .warning: return .systemOrange
        case .error: return .systemRed
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


    @ViewBuilder
    private var controlSection: some View {
        let stopping = supervisor.state.isActive

        // Stop needs a process, not a profile: a config broken or emptied while
        // the proxy serves used to leave Quit as the only way to stop it.
        Button {
            Task { await model.toggleProxy() }
        } label: {
            Label(stopping ? "Stop" : "Start", systemImage: stopping ? "stop.fill" : "play.fill")
        }
        .disabled(!stopping && model.selectedProfile == nil)

        // The lock for the selected profile. One the config already locks shows
        // checked and greyed: the menu can add a lock, never remove the file's.
        if let profile = model.selectedProfile {
            Toggle(isOn: Binding(
                get: { model.isReadOnly(profile) },
                set: { locked in Task { await model.setReadOnly(locked, for: profile) } }
            )) {
                Label("Read-only", systemImage: "lock")
            }
            .disabled(profile.readOnly)
            if profile.readOnly {
                Text("Set by read_only in the config")
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
        var label = profile.valid ? profile.name : "\(profile.name) (invalid)"
        if model.isReadOnly(profile) {
            label += " (read-only)"
        }
        return label
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
                    if let key = group.missingListenerKey {
                        // Otherwise the copy actions shrink to "Copy database
                        // name" with nothing to say why.
                        Text("Set \(key) in the profile to connect to these.")
                    }
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
        if supervisor.state.health == .checking {
            return "Checking Redash…"
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
        if let profile = model.connectionProfile {
            if source.wire == "postgres" {
                if let uri = ConnectionStrings.postgresURI(profile: profile, database: source.name) {
                    openInClientButton(uri: uri)
                }
                if let command = ConnectionStrings.psql(profile: profile, database: source.name) {
                    Button("Copy psql command") { Clipboard.copySecret(command) }
                }
                if let uri = ConnectionStrings.postgresURI(profile: profile, database: source.name) {
                    Button("Copy connection URI") { Clipboard.copySecret(uri) }
                }
            } else if source.wire == "mysql" {
                if let uri = ConnectionStrings.mysqlURI(profile: profile, database: source.name) {
                    openInClientButton(uri: uri)
                }
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


    /// Absent when nothing on this Mac claims the scheme, rather than a button
    /// that opens nothing.
    @ViewBuilder
    private func openInClientButton(uri: String) -> some View {
        if let handler = ClientApp.handler(for: uri) {
            Button("Open in \(handler.name)") { ClientApp.open(uri, with: handler) }
        }
    }


    @ViewBuilder
    private var connectSection: some View {
        if let profile = model.connectionProfile {
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
            set: { enabled in model.setLaunchAtLogin(enabled) }
        )) {
            Label("Launch at Login", systemImage: "power")
        }

        // Registered, but macOS holds it until you approve it. It used to read
        // as plain "off", with the toggle refusing to stay on.
        if model.launchAtLoginStatus == .requiresApproval {
            Text("Waiting for approval in System Settings › Login Items")
            Button("Open Login Items Settings…") { model.openLoginItemsSettings() }
        }
        if let error = model.launchAtLoginError {
            Text("Launch at Login failed: \(error)".fittedToMenu(limit: 80))
        }

        // The shortcuts only fire while the menu is open: LSUIElement leaves no app
        // menu to register a key equivalent with.
        Button {
            openSettings()
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


    /// Config deleted out from under us means there is nothing to edit yet.
    private func openSettings() {
        if !model.openConfigInEditor() {
            openWindow("onboarding")
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
