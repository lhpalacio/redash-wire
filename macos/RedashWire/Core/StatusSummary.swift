import Foundation

extension String {
    /// An NSMenu item is a single line that never wraps: the menu widens to fit
    /// the longest one, so anything that came from an error or from the config
    /// has to be bounded before it gets here.
    func fittedToMenu(limit: Int = 64) -> String {
        guard count > limit else { return self }
        return prefix(limit - 1).trimmingCharacters(in: .whitespaces) + "…"
    }
}

/// What the menu says about the proxy: one headline, at most two lines of
/// detail, and the actions that address it. Every state goes through here, so
/// they all read the same way.
struct StatusSummary: Equatable {
    enum Tone: Hashable {
        case idle, busy, ok, warning, error
    }

    enum Action: Hashable {
        case retry
        case checkNow
        case editConfiguration
        case showLogs
    }

    var tone: Tone
    var headline: String
    var details: [String]
    var actions: [Action]

    init(tone: Tone, headline: String, details: [String] = [], actions: [Action] = []) {
        self.tone = tone
        self.headline = headline
        self.details = details.map { $0.fittedToMenu() }
        self.actions = actions
    }

    init(_ tracker: ProxyTracker, now: Date) {
        switch tracker.state {
        case .stopped:
            self.init(tone: .idle, headline: "Stopped")

        case .starting:
            if let restart = tracker.pendingRestart {
                self.init(tone: .busy, headline: "Restarting after a crash",
                          details: ["Attempt \(restart.attempt) of \(restart.limit) in \(Self.countdown(to: restart.at, now: now))"])
            } else {
                self.init(tone: .busy, headline: "Starting…")
            }

        case .running(_, .ok):
            self.init(tone: .ok, headline: "Running", details: Self.listenerLines(tracker.activeProfile))

        case .running(_, .checking):
            self.init(tone: .busy, headline: "Connecting to Redash…")

        case .running(_, .unreachable):
            let cause = tracker.state.health?.summary ?? "Redash did not answer."
            if tracker.everConnected {
                let next = tracker.nextProbeAt.map { Self.nextAttempt(at: $0, now: now, prefix: "Next check") }
                self.init(tone: .warning, headline: "Redash offline",
                          details: ["\(cause) Check your VPN or network."] + [next].compactMap { $0 },
                          actions: [.checkNow])
            } else {
                var progress = tracker.failedProbes == 1 ? "Tried once" : "Tried \(tracker.failedProbes) times"
                if let at = tracker.nextProbeAt {
                    progress += " · " + Self.nextAttempt(at: at, now: now, prefix: "next try").lowercased()
                }
                self.init(tone: .busy, headline: "Connecting to Redash…", details: [cause, progress])
            }

        case .running(_, .rejected(let reason)), .gaveUp(.rejected(let reason)):
            let cause = RedashHealth.rejected(reason).summary ?? "Redash refused the request."
            self.init(tone: .error, headline: "Redash rejected the API key",
                      details: [cause, "Check the profile's API key and URL."],
                      actions: tracker.state.isRunning ? [.editConfiguration] : [.editConfiguration, .retry])

        case .gaveUp(let health):
            let cause = health.summary ?? "Redash did not answer."
            self.init(tone: .error, headline: "Can't reach Redash",
                      details: ["\(cause) Check your VPN or network.", "Tries again when your network changes."],
                      actions: [.retry, .showLogs])

        case .failed(let reason):
            self = Self.failure(reason)
        }
    }

    /// The daemon's reason is a Go error, often with the whole chain of causes.
    /// The common ones get a sentence; anything else is shown as written, cut
    /// to fit, with the log a click away.
    static func failure(_ reason: String) -> StatusSummary {
        let text = reason.lowercased()

        if text.contains("address already in use") {
            let headline = port(in: reason).map { "Port \($0) is already in use" } ?? "A port is already in use"
            return StatusSummary(tone: .error, headline: headline,
                                 details: ["Another app, or another redash-wire, is using it."],
                                 actions: [.retry, .editConfiguration])
        }
        if let range = reason.range(of: "loading config: ") {
            return StatusSummary(tone: .error, headline: "The config has a problem",
                                 details: [String(reason[range.upperBound...])],
                                 actions: [.editConfiguration])
        }
        if text.hasPrefix("panic:") || text.hasPrefix("fatal error:") {
            return StatusSummary(tone: .error, headline: "redash-wire crashed",
                                 details: [reason], actions: [.retry, .showLogs])
        }
        if text.contains("keeps stopping") {
            return StatusSummary(tone: .error, headline: "redash-wire keeps crashing",
                                 details: ["It stopped \(ProxyTracker.backoffDelays.count) times in a row."],
                                 actions: [.retry, .showLogs])
        }
        if text.hasPrefix("could not start") {
            return StatusSummary(tone: .error, headline: "Couldn't launch redash-wire",
                                 details: [reason], actions: [.showLogs])
        }
        return StatusSummary(tone: .error, headline: "redash-wire stopped",
                             details: [reason], actions: [.retry, .showLogs])
    }

    /// The daemon wraps a bind failure as "listening on <addr>: …".
    private static func port(in reason: String) -> String? {
        guard let start = reason.range(of: "listening on ") else { return nil }
        let address = reason[start.upperBound...].prefix { $0 != " " }.trimmingCharacters(in: CharacterSet(charactersIn: ":"))
        guard let colon = address.lastIndex(of: ":") else { return nil }
        let port = address[address.index(after: colon)...]
        return port.isEmpty || !port.allSatisfy(\.isNumber) ? nil : String(port)
    }

    private static func listenerLines(_ profile: Profile?) -> [String] {
        guard let profile else { return [] }
        var lines: [String] = []
        if !profile.postgresListenAddr.isEmpty {
            lines.append("PostgreSQL  \(profile.postgresListenAddr)")
        }
        if !profile.mysqlListenAddr.isEmpty {
            lines.append("MySQL  \(profile.mysqlListenAddr)")
        }
        return lines
    }

    /// Once the count reaches zero the probe is in flight for up to its
    /// timeout, which is not a number.
    private static func nextAttempt(at date: Date, now: Date, prefix: String) -> String {
        date.timeIntervalSince(now) > 0.5 ? "\(prefix) in \(countdown(to: date, now: now))" : "Trying again now…"
    }

    static func countdown(to date: Date, now: Date) -> String {
        let seconds = max(0, Int(date.timeIntervalSince(now).rounded(.up)))
        if seconds < 60 { return "\(seconds)s" }
        return "\(seconds / 60)m \(seconds % 60)s"
    }
}
