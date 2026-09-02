import XCTest
@testable import RedashWireCore

/// The tracker is the app's reading of the daemon's JSON stream and process
/// lifecycle. These tests feed it the events the daemon emits and check what
/// the menu would show, without a process anywhere.
final class ProxyTrackerTests: XCTestCase {
    private let t0 = Date(timeIntervalSince1970: 1_000_000)

    private func profile(postgres: String = "127.0.0.1:15432", mysql: String = "") -> Profile {
        Profile(
            name: "prod", redashURL: "https://redash.example.com", apiKeySet: true,
            postgresListenAddr: postgres, mysqlListenAddr: mysql,
            username: "redash-wire", password: "secret", defaultCredentials: true,
            pollInterval: "500ms", pollTimeout: "120s", valid: true, error: ""
        )
    }

    private func event(_ name: String?, level: LogEvent.Level = .info, message: String = "", fields: [String: String] = [:]) -> LogEvent {
        LogEvent(time: t0, level: level, event: name, message: message, fields: fields)
    }

    private func running() -> ProxyTracker {
        var tracker = ProxyTracker()
        tracker.start(profile())
        tracker.record(event(WireEvent.listenerReady), now: t0)
        tracker.record(event(WireEvent.redashUp), now: t0)
        return tracker
    }


    func testEveryConfiguredListenerMustBindBeforeRunning() {
        var tracker = ProxyTracker()
        tracker.start(profile(postgres: "127.0.0.1:15432", mysql: "127.0.0.1:13306"))

        tracker.record(event(WireEvent.listenerReady), now: t0)
        XCTAssertEqual(tracker.state, .starting, "one bound port out of two does not make the proxy usable")

        tracker.record(event(WireEvent.listenerReady), now: t0)
        XCTAssertTrue(tracker.state.isRunning)
    }

    func testBoundListenersAreCheckingUntilTheFirstProbeAnswers() {
        // The daemon binds before it asks Redash, so for a few seconds the ports
        // are open and nothing is known. Showing green there is the lie the
        // health work exists to prevent; showing amber would flash a slashed
        // icon on every healthy start.
        var tracker = ProxyTracker()
        tracker.start(profile())
        tracker.record(event(WireEvent.listenerReady), now: t0)
        XCTAssertEqual(tracker.state, .running(since: t0, redash: .checking))
        XCTAssertNil(tracker.state.health?.summary)

        tracker.record(event(WireEvent.redashUp), now: t0)
        XCTAssertEqual(tracker.state, .running(since: t0, redash: .ok))
    }

    func testHealthReportedBeforeBindingIsAppliedOnceRunning() {
        // A cold start under -wait-for-redash reports Redash down before the
        // listeners finish binding. Losing that report showed green over a proxy
        // that could not serve a query.
        var tracker = ProxyTracker()
        tracker.start(profile())
        tracker.record(event(WireEvent.redashDown, level: .error, fields: ["kind": "unreachable", "error": "dial tcp: i/o timeout", "retry_in_seconds": "10"]), now: t0)
        XCTAssertEqual(tracker.state, .starting)

        tracker.record(event(WireEvent.listenerReady), now: t0)
        XCTAssertEqual(tracker.state, .running(since: t0, redash: .unreachable("dial tcp: i/o timeout")))
        XCTAssertEqual(tracker.nextProbeAt, t0.addingTimeInterval(10))
    }

    func testARetryEventMovesTheCountdownWithoutTouchingTheSnapshot() {
        // The menu is rebuilt for every published change and a rebuild closes an
        // open submenu, so the one thing that changes every ten seconds while
        // Redash is away must stay out of what is published.
        var tracker = running()
        tracker.record(event(WireEvent.redashDown, level: .error, fields: ["kind": "unreachable", "error": "down", "retry_in_seconds": "10"]), now: t0)
        let before = tracker.snapshot

        let later = t0.addingTimeInterval(10)
        tracker.record(event(WireEvent.redashRetry, fields: ["kind": "unreachable", "error": "down", "retry_in_seconds": "10"]), now: later)

        XCTAssertEqual(tracker.nextProbeAt, later.addingTimeInterval(10))
        XCTAssertEqual(tracker.snapshot, before)
    }

    func testRecoveryClearsTheCountdownAndTurnsGreen() {
        var tracker = running()
        tracker.record(event(WireEvent.redashDown, level: .error, fields: ["kind": "rejected", "error": "status 401", "retry_in_seconds": "300"]), now: t0)
        XCTAssertEqual(tracker.state.health, .rejected("status 401"))
        XCTAssertEqual(tracker.nextProbeAt, t0.addingTimeInterval(300))

        tracker.record(event(WireEvent.redashUp), now: t0)
        XCTAssertEqual(tracker.state, .running(since: t0, redash: .ok))
        XCTAssertNil(tracker.nextProbeAt)
    }

    func testDataSourcesComeFromTheEventAndLeaveWithTheProcess() {
        var tracker = running()
        let payload = #"[{"id":1,"name":"Warehouse","type":"pg","wire":"postgres"},{"id":2,"name":"BQ","type":"bigquery","wire":""}]"#
        tracker.record(event(WireEvent.dataSources, fields: ["sources": payload, "count": "2"]), now: t0)

        XCTAssertEqual(tracker.dataSources.map(\.name), ["Warehouse", "BQ"])
        XCTAssertEqual(tracker.dataSources.map(\.isServable), [true, false])

        _ = tracker.exit(status: 0, stopRequested: true, now: t0)
        XCTAssertEqual(tracker.dataSources, [], "a stopped proxy serves nothing, so the menu must not list its sources")
    }

    func testAnExitBeforeBindingFailsWithTheDaemonsOwnReason() {
        // Nothing bound, so the cause is permanent and a retry would only hide
        // it. The reason is the daemon's headline plus its diagnosis, which is
        // what turned "loading config" into something a person can act on.
        var tracker = ProxyTracker()
        tracker.start(profile())
        tracker.record(event(nil, level: .error, message: "loading config", fields: ["error": "listen tcp 127.0.0.1:15432: address already in use"]), now: t0)

        XCTAssertEqual(tracker.exit(status: 1, stopRequested: false, now: t0), .failed)
        XCTAssertEqual(tracker.state, .failed("loading config: listen tcp 127.0.0.1:15432: address already in use"))
    }

    func testACrashAfterBindingRestartsWithBackoffThenGivesUp() {
        var tracker = running()
        let expected: [Duration] = [.seconds(1), .seconds(2), .seconds(4)]

        for (index, delay) in expected.enumerated() {
            let outcome = tracker.exit(status: 2, stopRequested: false, now: t0)
            XCTAssertEqual(outcome, .restart(profile(), after: delay), "attempt \(index + 1)")
            XCTAssertEqual(tracker.state, .starting)
            XCTAssertEqual(tracker.pendingRestart, .init(at: t0.addingTimeInterval(TimeInterval(delay.components.seconds)), attempt: index + 1, limit: 3))

            tracker.launch(profile())
            XCTAssertNil(tracker.pendingRestart, "the countdown must not outlive the wait it counts")
            tracker.record(event(WireEvent.listenerReady), now: t0)
        }

        XCTAssertEqual(tracker.exit(status: 2, stopRequested: false, now: t0), .failed)
        XCTAssertEqual(tracker.state, .failed("redash-wire keeps stopping after 3 restarts"))
    }

    func testAManualStartClearsTheRestartBudget() {
        var tracker = running()
        for _ in 0..<3 {
            _ = tracker.exit(status: 2, stopRequested: false, now: t0)
            tracker.launch(profile())
            tracker.record(event(WireEvent.listenerReady), now: t0)
        }
        XCTAssertEqual(tracker.exit(status: 2, stopRequested: false, now: t0), .failed)

        tracker.start(profile())
        tracker.record(event(WireEvent.listenerReady), now: t0)
        XCTAssertEqual(tracker.exit(status: 2, stopRequested: false, now: t0), .restart(profile(), after: .seconds(1)),
                       "a retry after three crashes must not be born exhausted")
    }

    func testOnlyAStableRunClearsTheRestartBudget() {
        // Binding is not proof the restart worked; a proxy that binds and dies a
        // second later would otherwise restart every second forever. Serving for
        // a while is.
        var tracker = running()
        _ = tracker.exit(status: 2, stopRequested: false, now: t0)
        tracker.launch(profile())
        tracker.record(event(WireEvent.listenerReady), now: t0)

        let soon = t0.addingTimeInterval(5)
        XCTAssertEqual(tracker.exit(status: 2, stopRequested: false, now: soon), .restart(profile(), after: .seconds(2)),
                       "a crash five seconds after binding is still the same streak")

        tracker.launch(profile())
        tracker.record(event(WireEvent.listenerReady), now: soon)
        let muchLater = soon.addingTimeInterval(ProxyTracker.stableRun)
        XCTAssertEqual(tracker.exit(status: 2, stopRequested: false, now: muchLater), .restart(profile(), after: .seconds(1)),
                       "a crash after a stable run starts a new streak")
    }

    func testARequestedStopIsAStopWhateverTheStatus() {
        var tracker = running()
        XCTAssertEqual(tracker.exit(status: 143, stopRequested: true, now: t0), .stopped)
        XCTAssertEqual(tracker.state, .stopped)
    }

    func testAPanicOutranksAnEarlierUnrelatedErrorAsTheReason() {
        // Redash went away, then the daemon crashed for a reason of its own.
        // The last error line is about Redash; the panic is why it stopped.
        var tracker = running()
        tracker.record(event(WireEvent.redashDown, level: .error, message: "redash is unreachable", fields: ["kind": "unreachable", "error": "dial tcp: connection refused"]), now: t0)
        tracker.record(.raw(line: "panic: runtime error: index out of range [5] with length 3", now: t0), now: t0)
        tracker.record(.raw(line: "goroutine 1 [running]:", now: t0), now: t0)
        tracker.record(.raw(line: "main.main()", now: t0), now: t0)

        for _ in 0..<3 {
            _ = tracker.exit(status: 2, stopRequested: false, now: t0)
            tracker.launch(profile())
            tracker.record(event(WireEvent.listenerReady), now: t0)
            tracker.record(.raw(line: "panic: runtime error: index out of range [5] with length 3", now: t0), now: t0)
        }
        XCTAssertEqual(tracker.exit(status: 2, stopRequested: false, now: t0), .failed)
        XCTAssertEqual(tracker.state, .failed("panic: runtime error: index out of range [5] with length 3"),
                       "the headline, not the trace that follows it")
    }

    func testProseBeforeTheLoggerExistsIsTheReasonOnlyWhenThereIsNoOther() {
        // The daemon prints "error: …" in plain text when it fails before its
        // JSON logger is set up. That used to be dropped, leaving only the
        // exit status.
        var tracker = ProxyTracker()
        tracker.start(profile())
        tracker.record(.raw(line: "error: unknown log format \"xml\"", now: t0), now: t0)
        _ = tracker.exit(status: 1, stopRequested: false, now: t0)
        XCTAssertEqual(tracker.state, .failed("error: unknown log format \"xml\""))

        tracker.start(profile())
        tracker.record(event(nil, level: .error, message: "loading config", fields: ["error": "no such profile"]), now: t0)
        tracker.record(.raw(line: "some stray line", now: t0), now: t0)
        _ = tracker.exit(status: 1, stopRequested: false, now: t0)
        XCTAssertEqual(tracker.state, .failed("loading config: no such profile"),
                       "a structured error outranks plain text that is not a crash")
    }
}

final class LogEventTests: XCTestCase {
    func testParsesTheDaemonsJSONLineAndStringifiesEveryOtherField() {
        // A real line from `-log-format json`. retry_in_seconds is a JSON number,
        // and the tracker reads it back through Double(), so it has to survive as
        // the digits rather than as a description of an NSNumber.
        let line = #"{"time":"2026-09-01T16:55:48-03:00","level":"error","msg":"redash is unreachable","event":"redash_down","kind":"unreachable","error":"dial tcp: connection refused","retry_in_seconds":10}"#
        let event = LogEvent.parse(line: Data(line.utf8))

        XCTAssertEqual(event?.level, .error)
        XCTAssertEqual(event?.event, "redash_down")
        XCTAssertEqual(event?.message, "redash is unreachable")
        XCTAssertEqual(event?.fields["retry_in_seconds"], "10")
        XCTAssertEqual(event?.fields["kind"], "unreachable")
        XCTAssertNil(event?.fields["msg"], "the headline is not a field")
        XCTAssertEqual(event?.time, ISO8601DateFormatter().date(from: "2026-09-01T16:55:48-03:00"))
    }

    func testALineThatIsNotAJSONObjectDoesNotParseButCanBeKeptRaw() {
        XCTAssertNil(LogEvent.parse(line: Data("panic: something".utf8)))
        XCTAssertNil(LogEvent.parse(line: Data("[1,2]".utf8)))

        // A Go panic used to vanish before it reached the log window, which
        // left a crash with no trace anywhere.
        let now = Date()
        let raw = LogEvent.raw(line: "panic: something", now: now)
        XCTAssertEqual(raw.level, .error)
        XCTAssertEqual(raw.message, "panic: something")
        XCTAssertTrue(raw.isRaw)
        XCTAssertNil(raw.event)
        XCTAssertFalse(LogEvent.parse(line: Data(#"{"msg":"x"}"#.utf8))?.isRaw ?? true)
    }

    func testAnUnknownLevelReadsAsInfo() {
        let event = LogEvent.parse(line: Data(#"{"level":"notice","msg":"x"}"#.utf8))
        XCTAssertEqual(event?.level, .info)
    }
}

final class RedashHealthTests: XCTestCase {
    func testTheMenuSummaryNeverCarriesTheGoErrorText() {
        // The daemon's error is a Go error with a full URL in it, and an NSMenu
        // item never wraps, so one such string stretches the menu across the
        // screen. The summary is a phrase; the original stays in the log window.
        let reason = #"fetching data sources: Get "https://redash.example.com/api/data_sources": context deadline exceeded"#
        let summary = RedashHealth.unreachable(reason).summary

        XCTAssertEqual(summary, "The request timed out.")
        XCTAssertFalse(summary?.contains("https://") ?? true)
    }

    func testARejectionNamesTheStatusAndPointsAtTheKey() {
        let health = RedashHealth(kind: "rejected", reason: "data sources request failed (status 401)")
        XCTAssertEqual(health.summary, "Redash answered with status 401.")
        XCTAssertEqual(health.remedy, "Check the profile's API key and URL.")
    }

    func testAnUnrecognisedReasonSaysOnlyWhatIsCertain() {
        XCTAssertEqual(RedashHealth.unreachable("something new").summary, "Redash did not answer.")
    }
}
