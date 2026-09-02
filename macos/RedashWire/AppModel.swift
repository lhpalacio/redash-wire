import AppKit
import Combine
import Foundation
import ServiceManagement

@MainActor
final class AppModel: ObservableObject {
    @Published private(set) var config: ConfigPayload?
    @Published private(set) var configError: WireError?
    @Published private(set) var selectedProfileName: String?
    /// Why the last Reload Configuration stopped the proxy rather than
    /// restarting it. Shown while it stays stopped.
    @Published private(set) var reloadNotice: String?
    @Published private(set) var launchAtLoginError: String?

    let cli: WireCLI
    let supervisor: ProxySupervisor
    let updates = UpdateChecker()

    private var watcher: ConfigWatcher?
    private var updateTask: Task<Void, Never>?
    private var didStart = false

    init(cli: WireCLI = .standard()) {
        self.cli = cli
        self.supervisor = ProxySupervisor(cli: cli)
    }


    var profiles: [Profile] { config?.profiles ?? [] }

    var isConfigured: Bool { config?.exists == true && !profiles.isEmpty }

    /// True only when the binary looked and found no config file. A config it
    /// could not read is a different problem, and the wizard's one answer to
    /// that — "a config already exists" — helps nobody; the menu shows the
    /// error and a way to the file instead.
    var needsOnboarding: Bool { config?.exists == false }

    /// The profile on disk that the next start uses.
    var selectedProfile: Profile? {
        guard let name = selectedProfileName else { return nil }
        return profiles.first { $0.name == name }
    }

    /// The profile whose ports and credentials the menu offers. While the proxy
    /// is up, it is the copy the process was launched from: the selection on
    /// disk may have been edited or renamed since, and its addresses would
    /// describe a listener nothing has bound. Otherwise it is what the next
    /// start will use.
    var connectionProfile: Profile? {
        supervisor.state.isActive ? supervisor.activeProfile : selectedProfile
    }

    /// What the status line names. The state belongs to the process, so this is
    /// the profile that process was launched from, or has just failed on; only
    /// a stopped proxy is described by the on-disk selection, which is what
    /// Start would run.
    var describedProfileName: String? {
        if supervisor.state == .stopped {
            return selectedProfileName
        }
        return supervisor.activeProfile?.name ?? selectedProfileName
    }

    /// Set while the proxy runs a profile the config no longer matches. Nil
    /// when the config could not be read at all: that is shown as an error, and
    /// the proxy keeps serving what it has.
    var runningProfileDrift: ProfileDrift? {
        guard supervisor.state.isActive, let running = supervisor.activeProfile, config != nil else { return nil }
        let drift = ProfileDrift(running: running, onDisk: profiles)
        return drift == .unchanged ? nil : drift
    }

    /// Only a running proxy has data sources. Asking Redash for them while the
    /// proxy is stopped means an API call nobody asked for, on a network that may
    /// not reach Redash at all, to populate a menu whose connection strings point
    /// at a port that is not listening.
    var dataSources: [DataSource] { supervisor.dataSources }

    var servableDataSources: [DataSource] { dataSources.filter(\.isServable) }
    var unservableDataSources: [DataSource] { dataSources.filter { !$0.isServable } }

    struct DataSourceGroup: Identifiable {
        let wire: String
        let title: String
        let sources: [DataSource]
        /// The config key that would turn the listener on, when the running
        /// profile has none for this wire. The proxy can serve these sources,
        /// but not on this profile, and the copy actions would have no port.
        let missingListenerKey: String?
        var id: String { wire }
    }

    /// Servable sources by the protocol they are served over, Postgres first.
    /// A wire this app does not know gets its raw name rather than being
    /// dropped, since a newer binary may add one.
    var dataSourceGroups: [DataSourceGroup] {
        let known = ["postgres", "mysql"]
        let byWire = Dictionary(grouping: servableDataSources, by: \.wire)
        let wires = known.filter { byWire[$0] != nil } + byWire.keys.filter { !known.contains($0) }.sorted()
        return wires.map { wire in
            let missing = Self.listenerKey(for: wire).flatMap { key in
                listenerAddress(for: wire).isEmpty ? key : nil
            }
            return DataSourceGroup(
                wire: wire,
                title: Self.wireTitle(wire) + (missing == nil ? "" : " (listener off)"),
                sources: byWire[wire] ?? [],
                missingListenerKey: missing
            )
        }
    }

    private func listenerAddress(for wire: String) -> String {
        switch wire {
        case "postgres": return connectionProfile?.postgresListenAddr ?? ""
        case "mysql": return connectionProfile?.mysqlListenAddr ?? ""
        default: return ""
        }
    }

    private static func listenerKey(for wire: String) -> String? {
        switch wire {
        case "postgres": return "postgres_listen_addr"
        case "mysql": return "mysql_listen_addr"
        default: return nil
        }
    }

    private static func wireTitle(_ wire: String) -> String {
        switch wire {
        case "postgres": return "PostgreSQL"
        case "mysql": return "MySQL"
        default: return wire
        }
    }


    func start() async {
        // The menu bar label's .task drives this. It runs once today, but a second
        // run would leave the first update loop running forever with nothing able
        // to reach it.
        guard !didStart else { return }
        didStart = true

        await reloadConfig()

        // A menu bar app launched at login exists to have the proxy up. Under
        // -wait-for-redash a missing VPN is a state the menu shows, not a reason
        // to hold back, so there is nothing to wait for here.
        if let profile = selectedProfile {
            await run(profile)
        }

        // Checks at launch and, for a menu bar app left running for weeks, once a
        // day after that. UpdateChecker decides whether enough time has passed.
        updateTask = Task { [weak self] in
            while !Task.isCancelled {
                await self?.updates.checkInBackground()
                try? await Task.sleep(for: .seconds(6 * 60 * 60))
            }
        }
    }

    /// Only the menu command restarts the proxy. A save while it serves would
    /// otherwise drop live connections, so the watcher only refreshes what the
    /// menu shows, and the menu says when a reload has something to apply.
    func reloadConfig(applyToRunning: Bool = false) async {
        let running = supervisor.state.isActive ? supervisor.activeProfile : nil
        await readConfig()
        watchConfigFile()
        guard applyToRunning else { return }
        reloadNotice = nil

        // A failed proxy is the one whose config you were editing to fix, and
        // reloading is the moment to find out whether it worked. A stopped one
        // stays stopped: that was a choice, not a failure.
        if case .failed = supervisor.state, let profile = selectedProfile {
            await supervisor.start(profile: profile)
            return
        }

        // A config that cannot be read is shown as an error. The proxy that is
        // up keeps serving the copy it has.
        guard let running, config != nil else { return }

        switch ProfileDrift(running: running, onDisk: profiles).reloadAction(fallback: selectedProfile) {
        case .keep:
            return
        case .restart(let profile):
            await supervisor.restart(profile: profile)
        case .stop:
            await supervisor.stop()
            reloadNotice = "Stopped: profile “\(running.name)” is no longer in the config."
        }
    }

    private func readConfig() async {
        do {
            let payload = try await cli.config()
            config = payload
            configError = nil

            let names = payload.profiles.map(\.name)
            guard !names.isEmpty else {
                selectedProfileName = nil
                return
            }

            // Editing the config must not switch profiles underneath you.
            if let current = selectedProfileName, names.contains(current) {
                return
            }
            selectedProfileName = names.contains(payload.defaultProfile)
                ? payload.defaultProfile
                : names.first
        } catch let error as WireError {
            configError = error
            config = nil
        } catch {
            configError = WireError(code: .unknown, message: error.localizedDescription)
            config = nil
        }
    }


    func toggleProxy() async {
        if supervisor.state.isActive {
            await supervisor.stop()
        } else if let profile = selectedProfile {
            await supervisor.start(profile: profile)
        }
    }

    func select(profile: Profile) async {
        guard profile.name != selectedProfileName else { return }
        selectedProfileName = profile.name
        await run(profile)
    }

    func runOnboarding(redashURL: String, profile: String, apiKey: String) async throws -> InitResult {
        let result = try await cli.initialize(redashURL: redashURL, profile: profile, apiKey: apiKey)
        await reloadConfig()
        selectedProfileName = result.profile
        if let profile = selectedProfile {
            await run(profile)
        }
        return result
    }

    /// Picking a profile means run it. A live proxy comes back up on the new one
    /// and publishes its data sources itself; a stopped or failed one is started,
    /// since switching away from a profile that failed is the usual way out of
    /// that state. An invalid profile is started too: the binary is the one that
    /// reads the config, so it reports the reason and the menu shows it as failed.
    private func run(_ profile: Profile) async {
        if supervisor.state.isActive {
            await supervisor.switchTo(profile: profile)
        } else {
            await supervisor.start(profile: profile)
        }
    }


    /// Tried on every reload, not once at launch: on a first run the directory
    /// does not exist yet, so the watcher cannot be made until onboarding has
    /// created it. A watcher whose directory was deleted is replaced the same way.
    private func watchConfigFile() {
        guard watcher == nil || watcher?.isStale == true else { return }
        watcher = ConfigWatcher(path: cli.configPath) { [weak self] in
            Task { @MainActor [weak self] in
                await self?.reloadConfig()
            }
        }
    }

    /// Returns false when there is no config yet, so the caller can open onboarding
    /// instead.
    ///
    /// A plain `NSWorkspace.open` hands the file to whatever claims `.yaml`, which on
    /// a clean install is nothing at all. Resolving the handler first makes the
    /// TextEdit fallback reachable.
    func openConfigInEditor() -> Bool {
        let url = URL(fileURLWithPath: cli.configPath)
        guard FileManager.default.fileExists(atPath: url.path) else { return false }

        let workspace = NSWorkspace.shared
        let editor = workspace.urlForApplication(toOpen: url)
            ?? workspace.urlForApplication(withBundleIdentifier: "com.apple.TextEdit")
        guard let editor else { return false }

        workspace.open([url], withApplicationAt: editor, configuration: NSWorkspace.OpenConfiguration())
        return true
    }


    var launchAtLoginStatus: SMAppService.Status {
        SMAppService.mainApp.status
    }

    /// Registered, whether or not the system has let it take effect yet. The
    /// toggle shows what was asked for; `requiresApproval` gets its own line.
    var launchesAtLogin: Bool {
        switch launchAtLoginStatus {
        case .enabled, .requiresApproval: return true
        default: return false
        }
    }

    /// Fails outside a signed bundle, which is normal in development. The
    /// failure used to be swallowed, and the toggle flipped back with no word why.
    func setLaunchAtLogin(_ enabled: Bool) {
        do {
            if enabled {
                try SMAppService.mainApp.register()
            } else {
                try SMAppService.mainApp.unregister()
            }
            launchAtLoginError = nil
        } catch {
            launchAtLoginError = error.localizedDescription
        }
        objectWillChange.send()
    }

    func openLoginItemsSettings() {
        SMAppService.openSystemSettingsLoginItems()
    }
}

/// Watches the directory and the file. The directory catches the atomic rename
/// most editors save with, which replaces the inode and leaves a file-level
/// watch pointed at nothing; the file catches an in-place write, which the
/// directory never sees. The file watch is re-armed after every directory
/// event, since that is when the inode may have changed.
private final class ConfigWatcher {
    private let path: String
    private let onChange: () -> Void
    private var directorySource: DispatchSourceFileSystemObject?
    private var fileSource: DispatchSourceFileSystemObject?
    private var pending: DispatchWorkItem?

    /// The directory itself was deleted or moved, so the descriptor points at
    /// nothing that will change again. `rm -rf ~/.redash-wire` and a second
    /// run through onboarding is what this looks like.
    private(set) var isStale = false

    /// One save is several events — a temp file, a rename, an editor's swap
    /// file coming and going — and each used to be a reload.
    private static let quietPeriod: TimeInterval = 0.3

    /// Nil when the directory does not exist yet, which is what a first run
    /// looks like. The caller tries again once onboarding has created it.
    init?(path: String, onChange: @escaping () -> Void) {
        self.path = path
        self.onChange = onChange

        let directory = (path as NSString).deletingLastPathComponent
        guard let source = Self.watch(directory, for: [.write, .rename, .delete], handler: { [weak self] events in
            guard let self else { return }
            if !events.isDisjoint(with: [.rename, .delete]) {
                self.isStale = true
            }
            self.rearmFile()
            self.changed()
        }) else { return nil }
        directorySource = source
        rearmFile()
    }

    deinit {
        pending?.cancel()
        directorySource?.cancel()
        fileSource?.cancel()
    }

    private func rearmFile() {
        fileSource?.cancel()
        fileSource = Self.watch(path, for: [.write, .extend, .rename, .delete], handler: { [weak self] _ in
            self?.changed()
        })
    }

    private func changed() {
        pending?.cancel()
        let work = DispatchWorkItem { [weak self] in self?.onChange() }
        pending = work
        DispatchQueue.main.asyncAfter(deadline: .now() + Self.quietPeriod, execute: work)
    }

    private static func watch(
        _ path: String,
        for events: DispatchSource.FileSystemEvent,
        handler: @escaping (DispatchSource.FileSystemEvent) -> Void
    ) -> DispatchSourceFileSystemObject? {
        let descriptor = open(path, O_EVTONLY)
        guard descriptor >= 0 else { return nil }

        let source = DispatchSource.makeFileSystemObjectSource(
            fileDescriptor: descriptor,
            eventMask: events,
            queue: .main
        )
        source.setEventHandler { [weak source] in
            handler(source?.data ?? [])
        }
        source.setCancelHandler { close(descriptor) }
        source.resume()
        return source
    }
}
