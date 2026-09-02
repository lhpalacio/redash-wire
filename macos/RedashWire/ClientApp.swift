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
    /// once per data source every time it opens, and the answer only changes
    /// when an app is installed or removed.
    private static var cache: [String: Handler?] = [:]

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

    /// Clears the credential later, but only if nothing else was copied since.
    /// Without the changeCount check this would wipe whatever came next.
    static func copySecret(_ value: String, clearAfter seconds: TimeInterval = 60) {
        let pasteboard = NSPasteboard.general
        pasteboard.clearContents()
        pasteboard.setString(value, forType: .string)
        pasteboard.setString(value, forType: concealedType)
        let stamp = pasteboard.changeCount

        DispatchQueue.main.asyncAfter(deadline: .now() + seconds) {
            guard NSPasteboard.general.changeCount == stamp else { return }
            NSPasteboard.general.clearContents()
        }
    }
}
