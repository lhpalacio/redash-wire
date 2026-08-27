import AppKit
import Foundation

/// Asks GitHub whether a newer release exists, and stops there.
///
/// It never downloads or installs anything. The app is ad-hoc signed and not
/// notarized, so an unattended update would mean handing macOS a binary neither
/// it nor you had a chance to vouch for — the same reason the README asks you to
/// clear the quarantine flag by hand. This points at the release page instead.
@MainActor
final class UpdateChecker: ObservableObject {
    struct Release: Equatable, Codable {
        let version: String
        let url: URL
        /// The release workflow builds the app in a job of its own, after the one
        /// that publishes the CLI archives. A release can exist with no macOS zip
        /// attached, and saying so beats sending you to a page that hasn't got one.
        let hasMacApp: Bool
    }

    @Published private(set) var available: Release?
    @Published private(set) var isChecking = false

    private static let latestRelease = URL(string: "https://api.github.com/repos/lhpalacio/redash-wire/releases/latest")!
    private static let releasesPage = URL(string: "https://github.com/lhpalacio/redash-wire/releases")!
    private static let lastCheckKey = "lastUpdateCheck"
    private static let knownReleaseKey = "lastKnownRelease"
    private static let interval: TimeInterval = 24 * 60 * 60

    /// Set from the tag by release.yml. A locally built app reports whatever
    /// MARKETING_VERSION says, which is genuinely behind once a release exists.
    static var currentVersion: String {
        Bundle.main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "0.0.0"
    }

    /// What the last check found, restored across launches. Without this the menu
    /// row disappears on any relaunch inside the check window: the next background
    /// check returns early on the timestamp, leaving `available` nil while the
    /// release it found is still sitting there. Re-tested against the running
    /// version, so the row also clears itself once you install the update.
    init() {
        guard
            let stored = UserDefaults.standard.data(forKey: Self.knownReleaseKey),
            let release = try? JSONDecoder().decode(Release.self, from: stored),
            Self.isNewer(release.version, than: Self.currentVersion)
        else { return }
        available = release
    }


    /// The menu command. It always answers, because a menu item that silently
    /// does nothing is the failure this whole change exists to stop repeating.
    func checkNow() async {
        isChecking = true
        defer { isChecking = false }

        do {
            let release = try await fetchLatest()
            if Self.isNewer(release.version, than: Self.currentVersion) {
                remember(release)
                present(release)
            } else {
                remember(nil)
                presentUpToDate()
            }
            stampCheck()
        } catch {
            present(failure: error)
        }
    }

    /// Runs at launch and once a day after. Deliberately silent: it adds a row to
    /// the menu and nothing else — no banner, no download, no prompt. A failure
    /// leaves the timestamp alone so the next launch tries again.
    func checkInBackground() async {
        if let last = UserDefaults.standard.object(forKey: Self.lastCheckKey) as? Date,
           Date().timeIntervalSince(last) < Self.interval {
            return
        }
        guard let release = try? await fetchLatest() else { return }

        remember(Self.isNewer(release.version, than: Self.currentVersion) ? release : nil)
        stampCheck()
    }

    func openReleasePage() {
        NSWorkspace.shared.open(available?.url ?? Self.releasesPage)
    }


    private struct LatestRelease: Decodable {
        let tagName: String
        let htmlURL: String
        let assets: [Asset]

        struct Asset: Decodable {
            let name: String
        }

        enum CodingKeys: String, CodingKey {
            case tagName = "tag_name"
            case htmlURL = "html_url"
            case assets
        }
    }

    private func fetchLatest() async throws -> Release {
        var request = URLRequest(url: Self.latestRelease)
        request.setValue("application/vnd.github+json", forHTTPHeaderField: "Accept")
        // GitHub refuses API requests that do not identify themselves.
        request.setValue("redash-wire/\(Self.currentVersion)", forHTTPHeaderField: "User-Agent")
        request.timeoutInterval = 15
        request.cachePolicy = .reloadIgnoringLocalCacheData

        let (data, response) = try await URLSession.shared.data(for: request)
        guard let http = response as? HTTPURLResponse else {
            throw UpdateError.unexpectedResponse
        }
        guard http.statusCode == 200 else {
            throw UpdateError.status(http.statusCode)
        }

        let payload = try JSONDecoder().decode(LatestRelease.self, from: data)
        guard let url = URL(string: payload.htmlURL) else {
            throw UpdateError.unexpectedResponse
        }

        return Release(
            version: Self.normalized(payload.tagName),
            url: url,
            hasMacApp: payload.assets.contains { $0.name.hasSuffix(".zip") && $0.name.contains("macos") }
        )
    }

    private func remember(_ release: Release?) {
        available = release

        guard let release, let encoded = try? JSONEncoder().encode(release) else {
            UserDefaults.standard.removeObject(forKey: Self.knownReleaseKey)
            return
        }
        UserDefaults.standard.set(encoded, forKey: Self.knownReleaseKey)
    }

    private func stampCheck() {
        UserDefaults.standard.set(Date(), forKey: Self.lastCheckKey)
    }


    /// Compared component by component as numbers: 0.10.0 is newer than 0.9.0,
    /// which a plain string comparison gets backwards.
    ///
    /// Pure arithmetic over two strings, so it is nonisolated: nothing here needs
    /// the main actor, and keeping it off makes it callable from a test harness.
    nonisolated static func isNewer(_ candidate: String, than current: String) -> Bool {
        let new = parts(candidate)
        let old = parts(current)

        for index in 0..<max(new.count, old.count) {
            let a = index < new.count ? new[index] : 0
            let b = index < old.count ? old[index] : 0
            if a != b { return a > b }
        }
        return false
    }

    nonisolated private static func parts(_ version: String) -> [Int] {
        // Stops at the first non-version character, so a `git describe` build
        // number like 0.2.0-3-gabc123 compares as the 0.2.0 it is based on.
        normalized(version)
            .prefix { $0.isNumber || $0 == "." }
            .split(separator: ".")
            .map { Int($0) ?? 0 }
    }

    nonisolated private static func normalized(_ version: String) -> String {
        var trimmed = version.trimmingCharacters(in: .whitespacesAndNewlines)
        if trimmed.first == "v" || trimmed.first == "V" {
            trimmed.removeFirst()
        }
        return trimmed
    }


    private enum UpdateError: LocalizedError {
        case status(Int)
        case unexpectedResponse

        var errorDescription: String? {
            switch self {
            case .status(let code):
                return "GitHub answered with status \(code)."
            case .unexpectedResponse:
                return "GitHub's answer could not be read."
            }
        }
    }

    private func present(_ release: Release) {
        let alert = NSAlert()
        alert.messageText = "redash-wire \(release.version) is available"
        alert.informativeText = release.hasMacApp
            ? "You're running \(Self.currentVersion)."
            : "You're running \(Self.currentVersion). This release has no macOS app attached yet, so the page may only offer the command-line builds."
        alert.addButton(withTitle: "View Release")
        alert.addButton(withTitle: "Later")

        if runModal(alert) == .alertFirstButtonReturn {
            NSWorkspace.shared.open(release.url)
        }
    }

    private func presentUpToDate() {
        let alert = NSAlert()
        alert.messageText = "You're up to date"
        alert.informativeText = "redash-wire \(Self.currentVersion) is the latest release."
        alert.addButton(withTitle: "OK")
        _ = runModal(alert)
    }

    private func present(failure error: Error) {
        let alert = NSAlert()
        alert.alertStyle = .warning
        alert.messageText = "Couldn't check for updates"
        alert.informativeText = error.localizedDescription
        alert.addButton(withTitle: "OK")
        alert.addButton(withTitle: "Open Releases")

        if runModal(alert) == .alertSecondButtonReturn {
            NSWorkspace.shared.open(Self.releasesPage)
        }
    }

    /// LSUIElement leaves the app inactive, so an alert would otherwise open
    /// behind whatever you were looking at.
    private func runModal(_ alert: NSAlert) -> NSApplication.ModalResponse {
        NSApplication.shared.activate(ignoringOtherApps: true)
        return alert.runModal()
    }
}
