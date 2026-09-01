import Combine
import Darwin
import Foundation
import Network

/// Owns the redash-wire child process, one profile at a time. What the process
/// means is decided by `ProxyTracker`; this class spawns, feeds and stops it,
/// and publishes the tracker's snapshot for the menu.
@MainActor
final class ProxySupervisor: ObservableObject {
    typealias State = ProxyTracker.State
    typealias PendingRestart = ProxyTracker.PendingRestart

    /// Longer than the daemon's own 5s force-exit timer, so SIGKILL never lands
    /// mid-shutdown.
    private static let terminationGrace: TimeInterval = 7

    /// Assigned only when it differs, because the menu is a real NSMenu that
    /// SwiftUI rebuilds on every published change, and a rebuild closes whatever
    /// submenu is open.
    @Published private(set) var snapshot = ProxyTracker.Snapshot()

    var state: State { snapshot.state }
    var activeProfile: Profile? { snapshot.activeProfile }
    /// Reported by the running proxy on every health probe, and emptied whenever
    /// that proxy goes away. It belongs to the process: it comes from the same
    /// registry that resolves the database name you connect with, so a list left
    /// behind by a proxy that has stopped would describe a port nothing answers.
    var dataSources: [DataSource] { snapshot.dataSources }
    var pendingRestart: PendingRestart? { snapshot.pendingRestart }

    /// Not published: see `ProxyTracker.nextProbeAt`. The menu reads it when it
    /// opens.
    private(set) var nextProbeAt: Date?

    /// Every line the child wrote. Its own object, so the log window is the only
    /// view that re-renders per line.
    let log = LogStore()

    private let cli: WireCLI
    private var tracker = ProxyTracker()
    private var process: Process?
    /// Held open deliberately: closing it triggers the child's -exit-on-stdin-eof.
    private var stdinPipe: Pipe?
    private var lineBuffer = Data()
    private var stopRequested = false
    private var restartTask: Task<Void, Never>?
    private var pathWatcher: NetworkPathWatcher?

    init(cli: WireCLI) {
        self.cli = cli
        pathWatcher = NetworkPathWatcher { [weak self] in
            Task { @MainActor [weak self] in self?.networkPathChanged() }
        }
    }


    func start(profile: Profile) {
        guard process == nil else { return }
        tracker.start(profile)
        spawn(profile)
    }

    func stop() async {
        restartTask?.cancel()
        restartTask = nil

        guard let process, process.isRunning else {
            tracker.stopped()
            stopRequested = false
            publish()
            return
        }

        stopRequested = true
        process.terminate()

        let deadline = Date().addingTimeInterval(Self.terminationGrace)
        while process.isRunning && Date() < deadline {
            try? await Task.sleep(for: .milliseconds(100))
        }
        if process.isRunning {
            kill(process.processIdentifier, SIGKILL)
        }
    }

    /// Pass `profile` to come back up on an edited copy. Restarting on the stale
    /// `activeProfile` keeps the old `enabledListenerCount`, so enabling a second
    /// listener would mark the proxy ready as soon as the first one binds.
    func restart(profile: Profile? = nil) async {
        guard let target = profile ?? activeProfile else { return }
        await stop()
        start(profile: target)
    }

    /// One profile at a time. Two would usually collide on the same ports.
    func switchTo(profile: Profile) async {
        await restart(profile: profile)
    }

    /// The proxy's own timer would notice within an interval. A path change is
    /// earlier evidence, so ask it to probe now: a VPN that just dropped shows as
    /// unreachable in a second or two, and one that just came back as green.
    ///
    /// Only a proxy that has bound its listeners gets the signal. The daemon
    /// registers its SIGUSR1 handler before it binds, and an unhandled SIGUSR1
    /// kills a process.
    private func networkPathChanged() {
        guard tracker.reachedReady, let process, process.isRunning else { return }
        kill(process.processIdentifier, SIGUSR1)
    }


    private func spawn(_ profile: Profile) {
        stopRequested = false
        lineBuffer.removeAll()
        publish()

        let process = Process()
        process.executableURL = cli.binaryURL
        process.arguments = cli.serveArguments(profile: profile.name)

        let stdin = Pipe()
        let stderr = Pipe()
        process.standardInput = stdin
        process.standardError = stderr
        process.standardOutput = FileHandle.nullDevice

        stderr.fileHandleForReading.readabilityHandler = { [weak self] handle in
            let data = handle.availableData
            guard !data.isEmpty else {
                handle.readabilityHandler = nil
                return
            }
            Task { @MainActor [weak self] in
                self?.ingest(data)
            }
        }

        process.terminationHandler = { [weak self] finished in
            let status = finished.terminationStatus
            Task { @MainActor [weak self] in
                self?.handleExit(status: status)
            }
        }

        do {
            try process.run()
        } catch {
            tracker.launchFailed("could not start \(cli.binaryURL.lastPathComponent): \(error.localizedDescription)")
            publish()
            return
        }

        self.process = process
        self.stdinPipe = stdin
    }


    private func ingest(_ data: Data) {
        lineBuffer.append(data)

        while let newline = lineBuffer.firstIndex(of: UInt8(ascii: "\n")) {
            let line = lineBuffer[lineBuffer.startIndex..<newline]
            lineBuffer.removeSubrange(lineBuffer.startIndex...newline)
            guard !line.isEmpty, let event = LogEvent.parse(line: Data(line)) else { continue }
            record(event)
        }
    }

    private func record(_ event: LogEvent) {
        log.append(event)

        // Everything the child wrote belongs in the log, but nothing it wrote may
        // change state once it has exited. stderr is drained on a different task
        // from terminationHandler, so the last buffered line can arrive after the
        // process is gone — and would otherwise report a dead proxy as running, or
        // refill the data sources handleExit just cleared.
        guard process != nil else { return }

        tracker.record(event, now: Date())
        publish()
    }

    private func handleExit(status: Int32) {
        process = nil
        stdinPipe = nil

        let outcome = tracker.exit(status: status, stopRequested: stopRequested, now: Date())
        stopRequested = false
        publish()

        guard case .restart(let profile, let delay) = outcome else { return }
        restartTask = Task { [weak self] in
            try? await Task.sleep(for: delay)
            guard !Task.isCancelled, let self else { return }
            self.tracker.launch(profile)
            self.spawn(profile)
        }
    }

    private func publish() {
        nextProbeAt = tracker.nextProbeAt
        if tracker.snapshot != snapshot {
            snapshot = tracker.snapshot
        }
    }
}

/// Reports each change to the Mac's network path. A VPN coming or going shows up
/// here as an interface appearing or disappearing, seconds before the proxy's
/// timer would find out on its own. The first update describes the path as it
/// already is, so it is swallowed.
private final class NetworkPathWatcher {
    private let monitor = NWPathMonitor()
    private var lastPath: NWPath?

    init(onChange: @escaping @Sendable () -> Void) {
        monitor.pathUpdateHandler = { [weak self] path in
            guard let self else { return }
            defer { self.lastPath = path }
            guard let last = self.lastPath, last != path else { return }
            onChange()
        }
        monitor.start(queue: DispatchQueue(label: "redash-wire.network-path"))
    }

    deinit {
        monitor.cancel()
    }
}
