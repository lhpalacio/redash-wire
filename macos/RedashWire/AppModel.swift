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

    func reloadConfig() async {
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

    func revealConfigInFinder() {
        NSWorkspace.shared.activateFileViewerSelecting([URL(fileURLWithPath: cli.configPath)])
    }

    func openConfigInEditor() {
        NSWorkspace.shared.open(URL(fileURLWithPath: cli.configPath))
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
