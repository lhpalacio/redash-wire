import Foundation

/// The proxy takes a Redash data source name as the database name.
enum ConnectionStrings {
    /// Split the way Go's net.SplitHostPort split the address the proxy bound:
    /// the port follows the last colon, and an IPv6 host is in brackets, which
    /// are not part of the host. A wildcard bind is not dialable, so it becomes
    /// loopback, matching what the proxy prints in its banner.
    static func hostAndPort(from listenAddr: String) -> (host: String, port: String)? {
        guard let separator = listenAddr.lastIndex(of: ":") else { return nil }
        var host = String(listenAddr[listenAddr.startIndex..<separator])
        let port = String(listenAddr[listenAddr.index(after: separator)...])
        guard !port.isEmpty else { return nil }

        if host.hasPrefix("[") && host.hasSuffix("]") {
            host = String(host.dropFirst().dropLast())
        } else if host.contains(":") {
            // `::1:15432` could be split anywhere; Go refuses it too.
            return nil
        }

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
        return "\(scheme)://\(user):\(password)@\(uriHost(host)):\(port)\(path)"
    }

    /// An IPv6 host goes back in brackets in a URL, where a bare `::1:15432`
    /// would be read as some other host and port. psql and mysql take it bare;
    /// psql rejects the brackets. A zone (`fe80::1%en0`) needs its `%` encoded.
    private static func uriHost(_ host: String) -> String {
        guard host.contains(":") else { return host }
        return "[" + host.replacingOccurrences(of: "%", with: "%25") + "]"
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
