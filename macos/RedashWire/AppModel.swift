import AppKit
import Combine
import Foundation
import ServiceManagement

@MainActor
final class AppModel: ObservableObject {
    @Published private(set) var config: ConfigPayload?
    @Published private(set) var configError: WireError?
    @Published private(set) var selectedProfileName: String?

    @Published private(set) var dataSources: [DataSource] = []
    @Published private(set) var dataSourcesError: WireError?
    @Published private(set) var isLoadingDataSources = false

    @Published var isShowingOnboarding = false

    let cli: WireCLI
    let supervisor: ProxySupervisor

    private var watcher: ConfigWatcher?

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

    var servableDataSources: [DataSource] { dataSources.filter(\.isServable) }
    var unservableDataSources: [DataSource] { dataSources.filter { !$0.isServable } }


    func start() async {
        await reloadConfig()
        watchConfigFile()
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
        dataSources = []
        dataSourcesError = nil

        // Picking a profile while stopped should not start anything.
        if supervisor.state.isRunning || supervisor.state.isBusy {
            await supervisor.switchTo(profile: profile)
        }
        await refreshDataSources()
    }

    func refreshDataSources() async {
        guard let profile = selectedProfile else { return }
        isLoadingDataSources = true
        defer { isLoadingDataSources = false }

        do {
            dataSources = try await cli.dataSources(profile: profile.name)
            dataSourcesError = nil
        } catch let error as WireError {
            dataSources = []
            dataSourcesError = error
        } catch {
            dataSources = []
            dataSourcesError = WireError(code: .unknown, message: error.localizedDescription)
        }
    }

    func runOnboarding(redashURL: String, profile: String, apiKey: String) async throws -> InitResult {
        let result = try await cli.initialize(redashURL: redashURL, profile: profile, apiKey: apiKey)
        await reloadConfig()
        selectedProfileName = result.profile
        await refreshDataSources()
        return result
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
