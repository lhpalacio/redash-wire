import AppKit
import Foundation

/// The proxy takes a Redash data source name as the database name.
enum ConnectionStrings {
    /// A wildcard bind is not dialable, so it becomes loopback, matching what the
    /// proxy prints in its banner.
    static func hostAndPort(from listenAddr: String) -> (host: String, port: String)? {
        guard let separator = listenAddr.lastIndex(of: ":") else { return nil }
        var host = String(listenAddr[listenAddr.startIndex..<separator])
        let port = String(listenAddr[listenAddr.index(after: separator)...])
        guard !port.isEmpty else { return nil }

        if host.isEmpty || host == "0.0.0.0" || host == "::" {
            host = "127.0.0.1"
        }
        return (host, port)
    }

    static func psql(profile: Profile, database: String?) -> String? {
        guard let (host, port) = hostAndPort(from: profile.postgresListenAddr) else { return nil }
        var command = "psql -h \(host) -p \(port) -U \(shellQuoted(profile.username))"
        if let database {
            command += " -d \(shellQuoted(database))"
        }
        return command
    }

    static func mysql(profile: Profile, database: String?) -> String? {
        guard let (host, port) = hostAndPort(from: profile.mysqlListenAddr) else { return nil }
        var command = "mysql -h \(host) -P \(port) -u \(shellQuoted(profile.username)) -p"
        if let database {
            command += " \(shellQuoted(database))"
        }
        return command
    }

    static func postgresURI(profile: Profile, database: String?) -> String? {
        uri(scheme: "postgresql", addr: profile.postgresListenAddr, profile: profile, database: database)
    }

    static func mysqlURI(profile: Profile, database: String?) -> String? {
        uri(scheme: "mysql", addr: profile.mysqlListenAddr, profile: profile, database: database)
    }

    private static func uri(scheme: String, addr: String, profile: Profile, database: String?) -> String? {
        guard let (host, port) = hostAndPort(from: addr) else { return nil }
        let user = urlEncoded(profile.username)
        let password = urlEncoded(profile.password)
        let path = database.map { "/" + urlEncoded($0) } ?? ""
        return "\(scheme)://\(user):\(password)@\(host):\(port)\(path)"
    }

    /// RFC 3986 unreserved set. Encoding by `.alphanumerics` alone escapes hyphens
    /// too, turning `redash-wire` into `redash%2Dwire`.
    private static let unreserved: CharacterSet = {
        var set = CharacterSet.alphanumerics
        set.insert(charactersIn: "-._~")
        return set
    }()

    private static func urlEncoded(_ value: String) -> String {
        value.addingPercentEncoding(withAllowedCharacters: unreserved) ?? value
    }

    /// Quoted so a name with spaces pastes as one argument.
    private static func shellQuoted(_ value: String) -> String {
        guard value.contains(where: { !$0.isLetter && !$0.isNumber && $0 != "-" && $0 != "_" && $0 != "." }) else {
            return value
        }
        return "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
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
