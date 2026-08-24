import Combine
import Darwin
import Foundation

/// Owns the redash-wire child process, one profile at a time.
@MainActor
final class ProxySupervisor: ObservableObject {
    enum State: Equatable {
        case stopped
        case starting
        case running(since: Date)
        case failed(String)

        var isRunning: Bool {
            if case .running = self { return true }
            return false
        }

        var isBusy: Bool { self == .starting }
    }

    /// Its length is also the attempt limit.
    private static let backoffDelays: [Duration] = [.seconds(1), .seconds(2), .seconds(4)]

    /// Longer than the daemon's own 5s force-exit timer, so SIGKILL never lands
    /// mid-shutdown.
    private static let terminationGrace: TimeInterval = 7

    private static let maxEvents = 5_000

    @Published private(set) var state: State = .stopped
    @Published private(set) var activeProfile: Profile?
    @Published private(set) var events: [LogEvent] = []

    private let cli: WireCLI
    private var process: Process?
    /// Held open deliberately: closing it triggers the child's -exit-on-stdin-eof.
    private var stdinPipe: Pipe?
    private var lineBuffer = Data()

    private var expectedListeners = 0
    private var seenListeners = 0
    private var reachedReady = false
    private var stopRequested = false
    private var restartAttempts = 0
    private var restartTask: Task<Void, Never>?
    private var lastErrorMessage: String?

    init(cli: WireCLI) {
        self.cli = cli
    }


    func start(profile: Profile) {
        guard process == nil else { return }
        // A manual start clears the budget, so a retry after three crashes is not
        // born exhausted.
        restartAttempts = 0
        launch(profile: profile)
    }

    func stop() async {
        restartTask?.cancel()
        restartTask = nil

        guard let process, process.isRunning else {
            finishStopped()
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

    func restart() async {
        guard let profile = activeProfile else { return }
        await stop()
        start(profile: profile)
    }

    /// One profile at a time. Two would usually collide on the same ports.
    func switchTo(profile: Profile) async {
        await stop()
        start(profile: profile)
    }

    func clearLog() {
        events.removeAll()
    }


    private func launch(profile: Profile) {
        activeProfile = profile
        expectedListeners = profile.enabledListenerCount
        seenListeners = 0
        reachedReady = false
        stopRequested = false
        lastErrorMessage = nil
        lineBuffer.removeAll()
        state = .starting

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
            state = .failed("could not start \(cli.binaryURL.lastPathComponent): \(error.localizedDescription)")
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
            guard !line.isEmpty, let event = Self.parse(line: Data(line)) else { continue }
            record(event)
        }
    }

    private func record(_ event: LogEvent) {
        events.append(event)
        if events.count > Self.maxEvents {
            events.removeFirst(events.count - Self.maxEvents)
        }

        if event.level >= .error {
            lastErrorMessage = event.message
        }

        // Every configured listener must report in. With both enabled, one bound
        // port does not make the proxy usable.
        if event.message.hasPrefix("listening (") {
            seenListeners += 1
            if !reachedReady && seenListeners >= max(expectedListeners, 1) {
                reachedReady = true
                restartAttempts = 0
                state = .running(since: Date())
            }
        }
    }

    private static func parse(line: Data) -> LogEvent? {
        guard
            let object = try? JSONSerialization.jsonObject(with: line),
            let record = object as? [String: Any]
        else { return nil }

        let message = record["msg"] as? String ?? ""
        let level = (record["level"] as? String).flatMap(LogEvent.Level.init(rawValue:)) ?? .info
        let time = (record["time"] as? String).flatMap(Self.timestampParser.date(from:)) ?? Date()

        var fields: [String: String] = [:]
        for (key, value) in record where !["msg", "level", "time"].contains(key) {
            fields[key] = String(describing: value)
        }

        return LogEvent(time: time, level: level, message: message, fields: fields)
    }

    private static let timestampParser: ISO8601DateFormatter = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        return formatter
    }()


    private func handleExit(status: Int32) {
        process = nil
        stdinPipe = nil

        if stopRequested {
            finishStopped()
            return
        }

        // Nothing bound, so the cause is permanent: a rejected key, a port in use,
        // an invalid profile. A retry loop would only hide it.
        guard reachedReady else {
            state = .failed(lastErrorMessage ?? "redash-wire stopped before it began listening (status \(status))")
            return
        }

        guard restartAttempts < Self.backoffDelays.count else {
            state = .failed(lastErrorMessage ?? "redash-wire keeps stopping after \(restartAttempts) restarts")
            return
        }

        let delay = Self.backoffDelays[restartAttempts]
        restartAttempts += 1
        state = .starting

        guard let profile = activeProfile else {
            state = .failed("redash-wire stopped and no profile is selected")
            return
        }

        restartTask = Task { [weak self] in
            try? await Task.sleep(for: delay)
            guard !Task.isCancelled else { return }
            self?.launch(profile: profile)
        }
    }

    private func finishStopped() {
        process = nil
        stdinPipe = nil
        stopRequested = false
        reachedReady = false
        seenListeners = 0
        state = .stopped
    }
}
