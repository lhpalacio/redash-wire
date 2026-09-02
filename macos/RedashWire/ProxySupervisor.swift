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

    /// How long after the exit to keep waiting for stderr to close. It closes
    /// when the last writer is gone, which is the child itself unless it left
    /// something behind holding the pipe.
    private static let drainGrace: TimeInterval = 1

    /// Assigned only when it differs, because the menu is a real NSMenu that
    /// SwiftUI rebuilds on every published change, and a rebuild closes whatever
    /// submenu is open.
    @Published private(set) var snapshot = ProxyTracker.Snapshot()

    var state: State { snapshot.state }
    /// The profile the process was launched from: a copy, and the only thing
    /// the running proxy's ports and credentials can be read from. The config
    /// on disk may have been edited or renamed since.
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

    /// Bumped per spawn and carried by everything a process reports back. stderr
    /// is drained on its own task, so a line from a process that has exited can
    /// reach the main actor after its successor was spawned, and would otherwise
    /// be read as the successor's.
    private var generation = 0

    /// An exit is handled once the process has terminated *and* its stderr has
    /// closed, in whichever order they happen. They arrive from different
    /// queues, and the last lines the process wrote — a panic and its trace —
    /// used to lose the race and land after the exit had been judged without
    /// them.
    private var exitStatus: Int32?
    private var stderrDrained = false

    /// Resumed by `handleExit`, the one place `process` is let go of.
    private var exitWaiters: [CheckedContinuation<Void, Never>] = []

    /// Lifecycle commands run one after another. A stop suspends until the exit
    /// is observed, and two commands in flight at once — a profile switch while
    /// a reload's restart still waits for the old process — used to have the
    /// second find `process` still set and quietly do nothing.
    private var commands: Task<Void, Never>?

    init(cli: WireCLI) {
        self.cli = cli
        pathWatcher = NetworkPathWatcher { [weak self] in
            Task { @MainActor [weak self] in self?.networkPathChanged() }
        }
    }


    func start(profile: Profile) async {
        await enqueue { self.startNow(profile) }
    }

    /// Returns once the exit has been observed, so a start that follows always
    /// spawns. It used to return as soon as `isRunning` turned false, which is
    /// before terminationHandler runs and clears `process`; a restart then found
    /// it still set and left the proxy stopped.
    func stop() async {
        await enqueue { await self.stopNow() }
    }

    /// Pass `profile` to come back up on an edited copy. Restarting on the stale
    /// `activeProfile` keeps the old `enabledListenerCount`, so enabling a second
    /// listener would mark the proxy ready as soon as the first one binds.
    func restart(profile: Profile? = nil) async {
        guard let target = profile ?? activeProfile else { return }
        await enqueue {
            await self.stopNow()
            self.startNow(target)
        }
    }

    /// One profile at a time. Two would usually collide on the same ports.
    func switchTo(profile: Profile) async {
        await restart(profile: profile)
    }

    private func enqueue(_ command: @escaping @MainActor () async -> Void) async {
        let previous = commands
        let task = Task { @MainActor in
            await previous?.value
            await command()
        }
        commands = task
        await task.value
    }

    private func startNow(_ profile: Profile) {
        guard process == nil else { return }
        tracker.start(profile)
        spawn(profile)
    }

    private func stopNow() async {
        restartTask?.cancel()
        restartTask = nil

        guard let process else {
            tracker.stopped()
            stopRequested = false
            publish()
            return
        }

        // Set before the exit is observed, so a crash that lands in this window
        // is a stop, not a failure: a proxy that served for an hour and died as
        // Stop was pressed used to be reported as failing before it listened.
        stopRequested = true
        if process.isRunning {
            process.terminate()
        }

        // Process reaps a killed child like any other, so terminationHandler
        // still runs and the exit is observed the same way.
        let killer = Task { @MainActor [weak self] in
            try? await Task.sleep(for: .seconds(Self.terminationGrace))
            guard !Task.isCancelled, let self, self.process === process else { return }
            kill(process.processIdentifier, SIGKILL)
        }
        await waitForExit()
        killer.cancel()
    }

    private func waitForExit() async {
        guard process != nil else { return }
        await withCheckedContinuation { exitWaiters.append($0) }
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
        exitStatus = nil
        stderrDrained = false
        generation += 1
        let generation = self.generation
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
                Task { @MainActor [weak self] in
                    self?.stderrClosed(generation: generation)
                }
                return
            }
            Task { @MainActor [weak self] in
                self?.ingest(data, generation: generation)
            }
        }

        process.terminationHandler = { [weak self] finished in
            let status = finished.terminationStatus
            Task { @MainActor [weak self] in
                self?.processTerminated(status: status, generation: generation)
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


    private func ingest(_ data: Data, generation: Int) {
        guard generation == self.generation, process != nil else { return }
        lineBuffer.append(data)

        while let newline = lineBuffer.firstIndex(of: UInt8(ascii: "\n")) {
            let line = lineBuffer[lineBuffer.startIndex..<newline]
            lineBuffer.removeSubrange(lineBuffer.startIndex...newline)
            guard !line.isEmpty else { continue }
            // A Go panic is not JSON, and dropping it left a crash with no
            // trace anywhere.
            let event = LogEvent.parse(line: Data(line))
                ?? LogEvent.raw(line: String(decoding: line, as: UTF8.self), now: Date())
            log.append(event)
            tracker.record(event, now: Date())
            publish()
        }
    }

    private func stderrClosed(generation: Int) {
        guard generation == self.generation else { return }
        stderrDrained = true
        if let status = exitStatus {
            handleExit(status: status)
        }
    }

    private func processTerminated(status: Int32, generation: Int) {
        guard generation == self.generation else { return }
        exitStatus = status
        if stderrDrained {
            handleExit(status: status)
            return
        }
        Task { @MainActor [weak self] in
            try? await Task.sleep(for: .seconds(Self.drainGrace))
            guard let self, generation == self.generation else { return }
            self.handleExit(status: status)
        }
    }

    private func handleExit(status: Int32) {
        // Both the drain and the grace timer get here; the first one wins.
        guard process != nil else { return }
        process = nil
        stdinPipe = nil

        let outcome = tracker.exit(status: status, stopRequested: stopRequested, now: Date())
        stopRequested = false
        publish()

        let waiters = exitWaiters
        exitWaiters = []
        for waiter in waiters {
            waiter.resume()
        }

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
