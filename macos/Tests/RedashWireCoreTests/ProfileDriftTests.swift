import XCTest
@testable import RedashWireCore

/// The proxy runs the profile it was launched with; the config on disk moves
/// on without it. This is the comparison the menu's hint and Reload
/// Configuration are built on.
final class ProfileDriftTests: XCTestCase {
    private func profile(name: String = "prod", postgres: String = "127.0.0.1:15432", password: String = "secret") -> Profile {
        Profile(
            name: name, redashURL: "https://redash.example.com", apiKeySet: true,
            postgresListenAddr: postgres, mysqlListenAddr: "",
            username: "redash-wire", password: password, defaultCredentials: false,
            pollInterval: "500ms", pollTimeout: "120s", valid: true, error: ""
        )
    }

    func testTheRunningCopyIsFoundByNameAndComparedByContent() {
        let running = profile()

        XCTAssertEqual(ProfileDrift(running: running, onDisk: [profile(name: "staging"), running]), .unchanged)

        let moved = profile(postgres: "127.0.0.1:25432")
        XCTAssertEqual(ProfileDrift(running: running, onDisk: [moved]), .edited(moved))

        let newPassword = profile(password: "rotated")
        XCTAssertEqual(ProfileDrift(running: running, onDisk: [newPassword]), .edited(newPassword))

        XCTAssertEqual(ProfileDrift(running: running, onDisk: [profile(name: "prod-renamed")]), .removed,
                       "a rename looks like a removal: the proxy is serving a name the config no longer has")
        XCTAssertEqual(ProfileDrift(running: running, onDisk: []), .removed)
    }

    func testReloadRestartsOnTheEditedCopyOrFallsBackToTheSelection() {
        let edited = profile(postgres: "127.0.0.1:25432")
        let fallback = profile(name: "staging")

        XCTAssertEqual(ProfileDrift.unchanged.reloadAction(fallback: fallback), .keep,
                       "an edit to some other profile is not a reason to restart this one")
        XCTAssertEqual(ProfileDrift.edited(edited).reloadAction(fallback: fallback), .restart(edited))
        XCTAssertEqual(ProfileDrift.removed.reloadAction(fallback: fallback), .restart(fallback))
        XCTAssertEqual(ProfileDrift.removed.reloadAction(fallback: nil), .stop,
                       "with nothing to run, a reload stops the proxy rather than leaving it on a phantom profile")
    }
}
