import AppKit
import Combine
import Foundation
import ServiceManagement

@MainActor
final class AppModel: ObservableObject {
    @Published private(set) var config: ConfigPayload?
    @Published private(set) var configError: WireError?
    @Published private(set) var selectedProfileName: String?

    @Published var isShowingOnboarding = false

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

    var selectedProfile: Profile? {
        guard let name = selectedProfileName else { return nil }
        return profiles.first { $0.name == name }
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
        /// The config key that would turn the listener on, when the selected
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
        case "postgres": return selectedProfile?.postgresListenAddr ?? ""
        case "mysql": return selectedProfile?.mysqlListenAddr ?? ""
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
        watchConfigFile()

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

    /// Only the menu command restarts the proxy. The config is written with a
    /// temp-file rename and editors leave swap files in the same directory, so the
    /// watcher fires several times per save; restarting on that would drop live
    /// connections.
    func reloadConfig(applyToRunning: Bool = false) async {
        let running = applyToRunning ? supervisor.activeProfile : nil
        await readConfig()

        // An edit to some other profile is not a reason to restart this one.
        guard
            let running,
            supervisor.state.isRunning || supervisor.state.isBusy,
            let edited = profiles.first(where: { $0.name == running.name }),
            edited != running
        else { return }

        await supervisor.restart(profile: edited)
    }

    private func readConfig() async {
        do {
            let payload = try await cli.config()
            config = payload
            configError = nil

            if !payload.exists || payload.profiles.isEmpty {
                isShowingOnboarding = true
                selectedProfileName = nil
                return
            }

            // Editing the config must not switch profiles underneath you.
            let names = payload.profiles.map(\.name)
            if let current = selectedProfileName, names.contains(current) {
                return
            }
            selectedProfileName = names.contains(payload.defaultProfile)
                ? payload.defaultProfile
                : names.first
        } catch let error as WireError {
            configError = error
            config = nil
            if error.code == .notConfigured {
                isShowingOnboarding = true
            }
        } catch {
            configError = WireError(code: .unknown, message: error.localizedDescription)
        }
    }


    func toggleProxy() async {
        if supervisor.state.isRunning || supervisor.state.isBusy {
            await supervisor.stop()
        } else if let profile = selectedProfile {
            supervisor.start(profile: profile)
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
        if supervisor.state.isRunning || supervisor.state.isBusy {
            await supervisor.switchTo(profile: profile)
        } else {
            supervisor.start(profile: profile)
        }
    }


    private func watchConfigFile() {
        let path = cli.configPath
        watcher = ConfigWatcher(path: path) { [weak self] in
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


    var launchesAtLogin: Bool {
        SMAppService.mainApp.status == .enabled
    }

    /// Throws when the app is not in a signed bundle, which is normal in development.
    func setLaunchAtLogin(_ enabled: Bool) throws {
        if enabled {
            try SMAppService.mainApp.register()
        } else {
            try SMAppService.mainApp.unregister()
        }
        objectWillChange.send()
    }
}

/// Watches the directory, not the file: an atomic rename replaces the inode and
/// leaves a file-level watch pointed at nothing.
private final class ConfigWatcher {
    private let source: DispatchSourceFileSystemObject?
    private let descriptor: CInt

    init?(path: String, onChange: @escaping () -> Void) {
        let directory = (path as NSString).deletingLastPathComponent
        descriptor = open(directory, O_EVTONLY)
        guard descriptor >= 0 else { return nil }

        let source = DispatchSource.makeFileSystemObjectSource(
            fileDescriptor: descriptor,
            eventMask: [.write, .rename, .delete],
            queue: .main
        )
        source.setEventHandler(handler: onChange)
        let fd = descriptor
        source.setCancelHandler { close(fd) }
        source.resume()
        self.source = source
    }

    deinit {
        source?.cancel()
    }
}
