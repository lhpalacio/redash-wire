import AppKit
import Foundation

/// The app that opens `postgresql://` or `mysql://` links on this Mac. TablePlus,
/// Postico and DBeaver all register the schemes, so one click on a data source
/// opens a connection with the database filled in; nothing here knows which one
/// is installed.
enum ClientApp {
    struct Handler: Equatable {
        let name: String
        let url: URL
    }

    /// Looked up once per scheme. Launch Services is quick, but the menu asks
    /// once per data source every time it opens. Only a find is kept: caching
    /// "none" hid the button from a client installed after launch.
    private static var cache: [String: Handler] = [:]

    static func handler(for uri: String) -> Handler? {
        guard let url = URL(string: uri), let scheme = url.scheme else { return nil }
        if let cached = cache[scheme] { return cached }

        let found = NSWorkspace.shared.urlForApplication(toOpen: url).map { appURL in
            Handler(name: displayName(of: appURL), url: appURL)
        }
        cache[scheme] = found
        return found
    }

    static func open(_ uri: String, with handler: Handler) {
        guard let url = URL(string: uri) else { return }
        NSWorkspace.shared.open([url], withApplicationAt: handler.url, configuration: NSWorkspace.OpenConfiguration())
    }

    private static func displayName(of appURL: URL) -> String {
        let info = Bundle(url: appURL)?.infoDictionary
        return (info?["CFBundleDisplayName"] as? String)
            ?? (info?["CFBundleName"] as? String)
            ?? appURL.deletingPathExtension().lastPathComponent
    }
}

enum Clipboard {
    /// The convention history tools honor to skip logging an item.
    private static let concealedType = NSPasteboard.PasteboardType("org.nspasteboard.ConcealedType")

    static func copy(_ value: String) {
        let pasteboard = NSPasteboard.general
        pasteboard.clearContents()
        pasteboard.setString(value, forType: .string)
    }

    /// The change count of a credential still waiting to be cleared.
    private static var pendingSecret: Int?

    /// Clears the credential later, but only if nothing else was copied since.
    /// Without the changeCount check this would wipe whatever came next. It
    /// stays on this Mac: Universal Clipboard would hand it to every device
    /// signed in to the same account.
    static func copySecret(_ value: String, clearAfter seconds: TimeInterval = 60) {
        let pasteboard = NSPasteboard.general
        pasteboard.prepareForNewContents(with: .currentHostOnly)
        pasteboard.setString(value, forType: .string)
        pasteboard.setString(value, forType: concealedType)
        let stamp = pasteboard.changeCount
        pendingSecret = stamp

        DispatchQueue.main.asyncAfter(deadline: .now() + seconds) {
            clearSecret(stamp)
        }
    }

    /// The timer dies with the app, so quitting inside the minute used to leave
    /// the credential behind.
    static func clearSecretOnQuit() {
        if let pendingSecret {
            clearSecret(pendingSecret)
        }
    }

    private static func clearSecret(_ stamp: Int) {
        if pendingSecret == stamp {
            pendingSecret = nil
        }
        guard NSPasteboard.general.changeCount == stamp else { return }
        NSPasteboard.general.clearContents()
    }
}
