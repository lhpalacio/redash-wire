import XCTest
@testable import RedashWireCore

/// What the menu shows for each state the daemon can be in: which colour, which
/// buttons, and the facts a person needs to act. Wording is free to change.
final class StatusSummaryTests: XCTestCase {
    private let t0 = Date(timeIntervalSince1970: 1_000_000)

    private func profile() -> Profile {
        Profile(
            name: "prod", redashURL: "https://redash.example.com", apiKeySet: true,
            postgresListenAddr: "127.0.0.1:15432", mysqlListenAddr: "",
            username: "redash-wire", password: "secret", defaultCredentials: true,
            pollInterval: "500ms", pollTimeout: "120s", readOnly: false, valid: true, error: ""
        )
    }

    private func event(_ name: String?, level: LogEvent.Level = .info, message: String = "", fields: [String: String] = [:]) -> LogEvent {
        LogEvent(time: t0, level: level, event: name, message: message, fields: fields)
    }

    private func down(_ kind: String = "unreachable", error: String = "dial tcp: i/o timeout", retryIn: Int = 10) -> LogEvent {
        event(WireEvent.redashDown, level: .error, fields: ["kind": kind, "error": error, "retry_in_seconds": "\(retryIn)"])
    }

    private func retry(retryIn: Int = 10) -> LogEvent {
        event(WireEvent.redashRetry, fields: ["kind": "unreachable", "error": "dial tcp: i/o timeout", "retry_in_seconds": "\(retryIn)"])
    }

    private func bound() -> ProxyTracker {
        var tracker = ProxyTracker()
        tracker.start(profile())
        tracker.record(event(WireEvent.listenerReady), now: t0)
        return tracker
    }

    func testAStartStillTryingSaysHowOftenAndWhenNext() {
        var tracker = bound()
        tracker.record(down(), now: t0)
        tracker.record(retry(), now: t0)
        tracker.record(retry(), now: t0)

        let summary = StatusSummary(tracker, now: t0)
        XCTAssertEqual(summary.tone, .busy, "still within its window, so not yet a problem to fix")
        XCTAssertTrue(summary.details.contains { $0.contains("3") && $0.contains("10s") }, "\(summary.details)")
        XCTAssertEqual(summary.actions, [])
    }

    func testAnOutageAfterServingOffersACheckNow() {
        var tracker = bound()
        tracker.record(event(WireEvent.redashUp), now: t0)
        tracker.record(down(retryIn: 100), now: t0)

        let summary = StatusSummary(tracker, now: t0)
        XCTAssertEqual(summary.tone, .warning)
        XCTAssertEqual(summary.actions, [.checkNow])
        XCTAssertTrue(summary.details.contains { $0.contains("1m 40s") }, "\(summary.details)")
    }

    func testAStartThatGaveUpOffersRetry() {
        var tracker = bound()
        tracker.gaveUp(.unreachable("dial tcp: i/o timeout"))

        let summary = StatusSummary(tracker, now: t0)
        XCTAssertEqual(summary.tone, .error)
        XCTAssertTrue(summary.actions.contains(.retry))
    }

    func testARejectedKeyPointsAtTheConfig() {
        var tracker = bound()
        tracker.record(down("rejected", error: "data sources request failed (status 401)", retryIn: 300), now: t0)

        let summary = StatusSummary(tracker, now: t0)
        XCTAssertEqual(summary.tone, .error)
        XCTAssertTrue(summary.actions.contains(.editConfiguration))
        XCTAssertTrue(summary.details.contains { $0.contains("401") }, "\(summary.details)")
    }

    func testAPortInUseIsNamedByItsPort() {
        let summary = StatusSummary.failure("server error: listening on 127.0.0.1:15432: listen tcp 127.0.0.1:15432: bind: address already in use")
        XCTAssertTrue(summary.headline.contains("15432"))
        XCTAssertFalse((summary.details + [summary.headline]).contains { $0.contains("bind:") }, "the Go error stays in the log")
        XCTAssertTrue(summary.actions.contains(.retry))
    }

    func testAConfigProblemShowsTheConfigsOwnMessage() {
        let summary = StatusSummary.failure(#"loading config: profile "prod": api_key is empty"#)
        XCTAssertEqual(summary.actions, [.editConfiguration])
        XCTAssertEqual(summary.details, [#"profile "prod": api_key is empty"#])
    }

    func testEveryDetailFitsOnOneMenuLine() {
        let long = "panic: " + String(repeating: "runtime error: index out of range ", count: 10)
        let summary = StatusSummary.failure(long)
        XCTAssertTrue(summary.details.allSatisfy { $0.count <= 64 }, "\(summary.details)")
        XCTAssertTrue(summary.actions.contains(.showLogs), "the full trace is in the log")
    }
}

final class StatusAlertTests: XCTestCase {
    private let running = StatusSummary(tone: .ok, headline: "Running")
    private let offline = StatusSummary(tone: .warning, headline: "Redash offline")
    private let connecting = StatusSummary(tone: .busy, headline: "Connecting to Redash…")
    private let stopped = StatusSummary(tone: .idle, headline: "Stopped")
    private let gaveUp = StatusSummary(tone: .error, headline: "Can't reach Redash")
    private let portInUse = StatusSummary(tone: .error, headline: "Port 15432 is already in use")

    func testGoingOfflineIsHeldBackAndCancelledByRecovery() {
        XCTAssertEqual(StatusAlert.changes(from: running, to: offline), [.offline])
        XCTAssertEqual(StatusAlert.changes(from: offline, to: running), [.backOnline])
    }

    func testStoppingWhileOfflineCancelsWithoutAWord() {
        XCTAssertEqual(StatusAlert.changes(from: offline, to: stopped), [.cancelOffline])
    }

    func testAProblemThatNeedsYouIsAlertedOncePerProblem() {
        XCTAssertEqual(StatusAlert.changes(from: connecting, to: gaveUp), [.needsAttention])
        XCTAssertEqual(StatusAlert.changes(from: gaveUp, to: gaveUp), [])
        XCTAssertEqual(StatusAlert.changes(from: gaveUp, to: portInUse), [.needsAttention])
    }

    func testAnOrdinaryStartIsSilent() {
        XCTAssertEqual(StatusAlert.changes(from: stopped, to: connecting), [])
        XCTAssertEqual(StatusAlert.changes(from: connecting, to: running), [])
    }
}
