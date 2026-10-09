import ServiceManagement
import SwiftUI

/// What used to be a row of toggles in the menu, plus what the menu had no room
/// for: every profile with its problems spelled out. The config file is still
/// edited as YAML; the binary stays its only writer.
struct SettingsView: View {
    @ObservedObject var model: AppModel
    @ObservedObject var supervisor: ProxySupervisor
    @ObservedObject var updates: UpdateChecker

    var body: some View {
        TabView {
            general
                .tabItem { Label("General", systemImage: "gearshape") }
            profiles
                .tabItem { Label("Profiles", systemImage: "rectangle.stack") }
            advanced
                .tabItem { Label("Advanced", systemImage: "wrench.and.screwdriver") }
        }
        .frame(width: 520, height: 420)
    }


    private var general: some View {
        Form {
            Section {
                Toggle("Launch at login", isOn: Binding(
                    get: { model.launchesAtLogin },
                    set: { model.setLaunchAtLogin($0) }
                ))
                // Registered, but macOS holds it until you approve it. It used to
                // read as plain "off", with the toggle refusing to stay on.
                if model.launchAtLoginStatus == .requiresApproval {
                    HStack {
                        Text("Waiting for your approval in Login Items.")
                            .foregroundStyle(.secondary)
                        Spacer()
                        Button("Open Login Items…") { model.openLoginItemsSettings() }
                    }
                }
                if let error = model.launchAtLoginError {
                    Text(error)
                        .foregroundStyle(.red)
                }
            }

            Section("Updates") {
                Toggle("Check for updates once a day", isOn: $updates.checksAutomatically)
                HStack {
                    Text("Version \(UpdateChecker.currentVersion)")
                        .foregroundStyle(.secondary)
                    Spacer()
                    Button("Check Now") {
                        Task { await updates.checkNow() }
                    }
                    .disabled(updates.isChecking)
                }
                if let release = updates.available {
                    HStack {
                        Text("\(release.version) is available.")
                        Spacer()
                        Button("Open Release Page") { updates.openReleasePage() }
                    }
                }
            }
        }
        .formStyle(.grouped)
    }


    private var profiles: some View {
        VStack(alignment: .leading, spacing: 0) {
            if let error = model.configError {
                Label(error.message, systemImage: "exclamationmark.triangle.fill")
                    .foregroundStyle(.red)
                    .padding()
            }
            List(model.profiles) { profile in
                ProfileRow(
                    profile: profile,
                    isSelected: profile.name == model.selectedProfileName,
                    isDefault: profile.name == model.config?.defaultProfile,
                    menuLocked: model.prefersReadOnly(profile)
                )
            }
            .overlay {
                if model.profiles.isEmpty && model.configError == nil {
                    Text("No profiles yet.")
                        .foregroundStyle(.secondary)
                }
            }
            Divider()
            HStack {
                Text(model.cli.configPath.replacingOccurrences(of: NSHomeDirectory(), with: "~"))
                    .foregroundStyle(.secondary)
                    .lineLimit(1)
                    .truncationMode(.middle)
                Spacer()
                Button("Show in Finder") { model.revealConfigInFinder() }
                Button("Edit…") {
                    if !model.openConfigInEditor() {
                        WindowPresenter.shared.show("onboarding")
                    }
                }
                Button("Reload") {
                    Task { await model.reloadConfig(applyToRunning: true) }
                }
            }
            .padding(10)
        }
    }


    private var advanced: some View {
        Form {
            Section {
                Toggle("Verbose logging", isOn: $model.verboseLogging)
                Text("Adds debug lines to the log, such as each query sent to Redash. Applies the next time the proxy starts.")
                    .font(.callout)
                    .foregroundStyle(.secondary)
                if supervisor.state.isActive {
                    Button("Restart Proxy Now") {
                        Task { await model.restartProxy() }
                    }
                }
            }
            Section("About") {
                LabeledContent("App", value: UpdateChecker.currentVersion)
                LabeledContent("redash-wire binary", value: model.binaryVersion ?? "not found")
                LabeledContent("Binary path", value: model.cli.binaryURL.path)
                    .lineLimit(1)
                    .truncationMode(.middle)
            }
        }
        .formStyle(.grouped)
    }
}

private struct ProfileRow: View {
    let profile: Profile
    let isSelected: Bool
    let isDefault: Bool
    let menuLocked: Bool

    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                Text(profile.name)
                    .font(.headline)
                if isDefault {
                    Text("default")
                        .font(.caption)
                        .foregroundStyle(.secondary)
                }
                Spacer()
                if isSelected {
                    Image(systemName: "checkmark")
                        .foregroundStyle(.secondary)
                        .accessibilityLabel("Selected")
                }
            }
            Text(profile.redashURL.isEmpty ? "No Redash URL" : profile.redashURL)
                .foregroundStyle(.secondary)
            Text(listeners)
                .font(.callout)
                .foregroundStyle(.secondary)
            if let lock {
                Label(lock, systemImage: "lock")
                    .font(.callout)
                    .foregroundStyle(.secondary)
            }
            if !profile.apiKeySet {
                Label("No API key", systemImage: "key")
                    .font(.callout)
                    .foregroundStyle(.orange)
            }
            if !profile.valid, !profile.error.isEmpty {
                Label(profile.error, systemImage: "exclamationmark.triangle.fill")
                    .font(.callout)
                    .foregroundStyle(.red)
                    .fixedSize(horizontal: false, vertical: true)
            }
        }
        .padding(.vertical, 4)
    }

    private var listeners: String {
        let postgres = profile.postgresListenAddr.isEmpty ? "off" : profile.postgresListenAddr
        let mysql = profile.mysqlListenAddr.isEmpty ? "off" : profile.mysqlListenAddr
        return "PostgreSQL \(postgres) · MySQL \(mysql)"
    }

    private var lock: String? {
        if profile.readOnly { return "Read-only, set by read_only in the config" }
        if menuLocked { return "Read-only, locked from the menu" }
        return nil
    }
}
