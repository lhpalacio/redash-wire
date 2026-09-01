import Foundation

/// The proxy's state as the app understands it, reduced from the JSON log stream
/// and the child process's lifecycle. It owns no process and reads no clock of
/// its own, which is what makes it testable: `ProxySupervisor` feeds it what
/// happened and publishes its snapshot.
struct ProxyTracker: Equatable {
    enum State: Equatable {
        case stopped
        case starting
        /// Running says the listeners are bound; `redash` says whether anything
        /// behind them can be served. The two are separate facts, and conflating
        /// them is what let the menu show green while every query failed.
        case running(since: Date, redash: RedashHealth)
        case failed(String)

        var isRunning: Bool {
            if case .running = self { return true }
            return false
        }

        var isBusy: Bool { self == .starting }

        /// Nil unless running: a stopped proxy is not asking Redash anything.
        var health: RedashHealth? {
            if case .running(_, let redash) = self { return redash }
            return nil
        }
    }

    /// A restart scheduled after a crash, with which attempt it is.
    struct PendingRestart: Equatable {
        let at: Date
        let attempt: Int
        let limit: Int
    }

    /// What the menu renders, and the only part the supervisor publishes. Nothing
    /// in it changes more often than the menu should be rebuilt.
    struct Snapshot: Equatable {
        var state: State = .stopped
        var activeProfile: Profile?
        var dataSources: [DataSource] = []
        var pendingRestart: PendingRestart?
    }

    /// What the supervisor has to do once the process is gone.
    enum Exit: Equatable {
        case stopped
        case failed
        case restart(Profile, after: Duration)
    }

    /// Its length is also the attempt limit.
    static let backoffDelays: [Duration] = [.seconds(1), .seconds(2), .seconds(4)]

    /// A proxy that served this long before dying has proved the last restart
    /// worked, so its next crash starts a new streak. The budget used to reset
    /// the moment the listeners bound, which let a proxy that bound and died a
    /// second later restart every second forever.
    static let stableRun: TimeInterval = 60

    private(set) var snapshot = Snapshot()

    /// When the running proxy will next ask Redash, as it said on its last failed
    /// probe. Nil while Redash is answering.
    ///
    /// Outside the snapshot on purpose. It moves on every retry event, and the
    /// menu is rebuilt for every published change; it is read when the menu
    /// opens, which is when anyone looks at it.
    private(set) var nextProbeAt: Date?

    /// Bound listeners are what make the proxy usable, and what make a later
    /// exit a crash rather than a failure to start.
    private(set) var reachedReady = false

    /// The latest health the daemon reported, held here because it can arrive
    /// before or after the listeners bind, and applied whenever the state is
    /// running. Nothing has answered at launch, so it starts as checking.
    private var reportedHealth: RedashHealth = .checking
    private var expectedListeners = 0
    private var seenListeners = 0
    private var restartAttempts = 0
    private var lastErrorMessage: String?

    var state: State { snapshot.state }
    var activeProfile: Profile? { snapshot.activeProfile }
    var dataSources: [DataSource] { snapshot.dataSources }
    var pendingRestart: PendingRestart? { snapshot.pendingRestart }


    /// A manual start clears the budget, so a retry after three crashes is not
    /// born exhausted.
    mutating func start(_ profile: Profile) {
        restartAttempts = 0
        launch(profile)
    }

    /// The process is being spawned for `profile`, whether by a start or by a
    /// scheduled restart.
    mutating func launch(_ profile: Profile) {
        snapshot.activeProfile = profile
        expectedListeners = profile.enabledListenerCount
        seenListeners = 0
        reachedReady = false
        reportedHealth = .checking
        lastErrorMessage = nil
        snapshot.dataSources = []
        nextProbeAt = nil
        snapshot.pendingRestart = nil
        snapshot.state = .starting
    }

    /// The spawn itself failed, so there is no process and nothing to wait for.
    mutating func launchFailed(_ reason: String) {
        snapshot.state = .failed(reason)
    }

    /// One line from a process that is still alive. The supervisor drops lines
    /// that arrive after the exit, so this never resurrects a dead proxy.
    mutating func record(_ event: LogEvent, now: Date) {
        if event.level >= .error {
            lastErrorMessage = Self.reason(for: event)
        }

        switch event.event {
        case WireEvent.listenerReady:
            // Every configured listener must report in. With both enabled, one
            // bound port does not make the proxy usable.
            seenListeners += 1
            if !reachedReady && seenListeners >= max(expectedListeners, 1) {
                reachedReady = true
                snapshot.state = .running(since: now, redash: reportedHealth)
            }

        case WireEvent.redashDown:
            reportedHealth = RedashHealth(
                kind: event.fields["kind"],
                reason: event.fields["error"] ?? event.message
            )
            nextProbeAt = Self.retryDate(from: event, now: now)
            applyHealth()

        case WireEvent.redashRetry:
            nextProbeAt = Self.retryDate(from: event, now: now)

        case WireEvent.redashUp:
            reportedHealth = .ok
            nextProbeAt = nil
            applyHealth()

        case WireEvent.dataSources:
            guard
                let payload = event.fields["sources"],
                let sources = try? JSONDecoder().decode([DataSource].self, from: Data(payload.utf8))
            else { return }
            snapshot.dataSources = sources

        default:
            break
        }
    }

    /// The process is gone. Decides between a clean stop, a permanent failure and
    /// a backoff restart, and says which so the supervisor can schedule it.
    mutating func exit(status: Int32, stopRequested: Bool, now: Date) -> Exit {
        // Whatever happens next — a backoff restart, a failure, a clean stop —
        // nothing is serving these any more.
        snapshot.dataSources = []
        nextProbeAt = nil

        if stopRequested {
            stopped()
            return .stopped
        }

        // Nothing bound, so the cause is permanent: a rejected key, a port in use,
        // an invalid profile. A retry loop would only hide it.
        guard reachedReady else {
            snapshot.state = .failed(lastErrorMessage ?? "redash-wire stopped before it began listening (status \(status))")
            return .failed
        }

        if case .running(let since, _) = snapshot.state, now.timeIntervalSince(since) >= Self.stableRun {
            restartAttempts = 0
        }

        guard restartAttempts < Self.backoffDelays.count else {
            snapshot.state = .failed(lastErrorMessage ?? "redash-wire keeps stopping after \(restartAttempts) restarts")
            return .failed
        }

        guard let profile = snapshot.activeProfile else {
            snapshot.state = .failed("redash-wire stopped and no profile is selected")
            return .failed
        }

        let delay = Self.backoffDelays[restartAttempts]
        restartAttempts += 1
        snapshot.state = .starting
        snapshot.pendingRestart = PendingRestart(
            at: now.addingTimeInterval(TimeInterval(delay.components.seconds)),
            attempt: restartAttempts,
            limit: Self.backoffDelays.count
        )
        return .restart(profile, after: delay)
    }

    /// A requested stop has completed, or there was nothing running to stop.
    mutating func stopped() {
        reachedReady = false
        seenListeners = 0
        snapshot.dataSources = []
        nextProbeAt = nil
        snapshot.pendingRestart = nil
        snapshot.state = .stopped
    }


    private mutating func applyHealth() {
        guard case .running(let since, let current) = snapshot.state, current != reportedHealth else { return }
        snapshot.state = .running(since: since, redash: reportedHealth)
    }

    /// slog puts the headline in `msg` and the diagnosis in `error`. Keeping only
    /// the message is what turned "cannot reach Redash: dial tcp ... i/o timeout"
    /// into a bare "fetching data sources" in the menu.
    private static func reason(for event: LogEvent) -> String {
        guard let detail = event.fields["error"], !detail.isEmpty else { return event.message }
        return "\(event.message): \(detail)"
    }

    /// Counted from receipt rather than the event's own timestamp, which has
    /// whole-second precision and would start the countdown up to a second off.
    private static func retryDate(from event: LogEvent, now: Date) -> Date? {
        guard let raw = event.fields["retry_in_seconds"], let seconds = Double(raw) else { return nil }
        return now.addingTimeInterval(seconds)
    }
}
