import Foundation

// Golden files in cmd/redash-wire/testdata pin these field names. Keys are spelled
// out because convertFromSnakeCase maps `redash_url` to `redashUrl`, which misses a
// `redashURL` property and fails to decode at runtime.

struct ConfigPayload: Decodable {
    let configPath: String
    let exists: Bool
    let source: String
    let defaultProfile: String
    let profiles: [Profile]

    enum CodingKeys: String, CodingKey {
        case configPath = "config_path"
        case exists
        case source
        case defaultProfile = "default_profile"
        case profiles
    }
}

struct Profile: Decodable, Identifiable, Equatable {
    let name: String
    let redashURL: String
    let apiKeySet: Bool
    /// Empty means the listener is disabled for this profile.
    let postgresListenAddr: String
    let mysqlListenAddr: String
    let username: String
    let password: String
    let defaultCredentials: Bool
    let pollInterval: String
    let pollTimeout: String
    /// Resolved from the config, so the menu can show it but not turn it off.
    let readOnly: Bool
    let valid: Bool
    let error: String

    var id: String { name }

    enum CodingKeys: String, CodingKey {
        case name
        case redashURL = "redash_url"
        case apiKeySet = "api_key_set"
        case postgresListenAddr = "postgres_listen_addr"
        case mysqlListenAddr = "mysql_listen_addr"
        case username
        case password
        case defaultCredentials = "default_credentials"
        case pollInterval = "poll_interval"
        case pollTimeout = "poll_timeout"
        case readOnly = "read_only"
        case valid
        case error
    }

    /// The supervisor waits for this many `listening` events before it calls the
    /// proxy ready.
    var enabledListenerCount: Int {
        [postgresListenAddr, mysqlListenAddr].filter { !$0.isEmpty }.count
    }
}

/// How the profile a proxy was launched from relates to the config on disk now.
/// The daemon reads its profile once, at launch, so an edit or a rename leaves
/// it serving a copy the config no longer contains. The menu has to describe
/// that copy, not the one on disk, and Reload Configuration has to know what to
/// bring the proxy back up on.
enum ProfileDrift: Equatable {
    case unchanged
    /// Still there under the same name, with something different in it.
    case edited(Profile)
    /// Renamed or deleted.
    case removed

    init(running: Profile, onDisk: [Profile]) {
        guard let current = onDisk.first(where: { $0.name == running.name }) else {
            self = .removed
            return
        }
        self = current == running ? .unchanged : .edited(current)
    }

    enum ReloadAction: Equatable {
        case keep
        case restart(Profile)
        case stop
    }

    /// What a reload does to the running proxy. `fallback` is the on-disk
    /// selection, which is what the proxy comes back up on when its own profile
    /// is gone; with nothing to fall back on, it stops.
    func reloadAction(fallback: Profile?) -> ReloadAction {
        switch self {
        case .unchanged:
            return .keep
        case .edited(let profile):
            return .restart(profile)
        case .removed:
            return fallback.map { .restart($0) } ?? .stop
        }
    }
}

struct DataSource: Decodable, Identifiable, Equatable {
    let id: Int
    let name: String
    let type: String
    /// "postgres", "mysql", or empty when the proxy cannot serve this source.
    let wire: String

    var isServable: Bool { !wire.isEmpty }
}

struct InitResult: Decodable {
    let configPath: String
    let profile: String
    let redashURL: String
    let postgresEnabled: Bool
    let mysqlEnabled: Bool
    let readOnly: Bool
    let dataSources: Int
    let userName: String
    let userEmail: String
    let redashVersion: String

    enum CodingKeys: String, CodingKey {
        case configPath = "config_path"
        case profile
        case redashURL = "redash_url"
        case postgresEnabled = "postgres_enabled"
        case mysqlEnabled = "mysql_enabled"
        case readOnly = "read_only"
        case dataSources = "data_sources"
        case userName = "user_name"
        case userEmail = "user_email"
        case redashVersion = "redash_version"
    }
}

/// Unknown values decode to `.unknown` instead of throwing: the contract allows
/// new codes, and an older app must survive a newer binary.
enum CLIErrorCode: String, Decodable {
    case usage
    case notConfigured = "not_configured"
    case invalidConfig = "invalid_config"
    case profileNotFound = "profile_not_found"
    case connectionFailed = "connection_failed"
    case authenticationFailed = "authentication_failed"
    case configExists = "config_exists"
    case ioError = "io_error"
    case unknown

    init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = CLIErrorCode(rawValue: raw) ?? .unknown
    }
}

struct CLIErrorPayload: Decodable {
    struct Body: Decodable {
        let code: CLIErrorCode
        let message: String
    }
    let error: Body
}

/// `code` is what the UI switches on. `message` is shown, never parsed.
struct WireError: LocalizedError {
    let code: CLIErrorCode
    let message: String

    var errorDescription: String? { message }

    var remedy: String? {
        switch code {
        case .notConfigured:
            return "Set up a connection to Redash to get started."
        case .authenticationFailed:
            return "Check the API key, and that the URL points at your Redash."
        case .connectionFailed:
            return "Redash could not be reached. Check the URL and your network."
        case .invalidConfig, .profileNotFound:
            return "Edit the config file to fix the problem, then reload."
        case .configExists:
            return "A config already exists. Edit it directly instead."
        case .usage, .ioError, .unknown:
            return nil
        }
    }
}

/// The stable machine names the daemon puts in each JSON log line's `event`
/// field. The app switches on these instead of on the message text, which used
/// to mean a reworded log sentence could silently break the menu.
enum WireEvent {
    static let listenerReady = "listener_ready"
    static let redashUp = "redash_up"
    static let redashDown = "redash_down"
    static let redashRetry = "redash_retry"
    static let dataSources = "datasources_refreshed"
}

/// How Redash is answering, as reported by the daemon's health events. It hangs
/// off the running state rather than standing alone, because a stopped proxy has
/// no opinion about Redash: nothing is asking it.
enum RedashHealth: Equatable {
    /// The listeners are bound but no probe has answered yet. The proxy binds
    /// before it asks under -wait-for-redash, so this is what a start looks like
    /// for the first few seconds; it is neither green nor amber.
    case checking
    case ok
    /// A network problem, a timeout, or a 5xx. Expected to clear on its own.
    case unreachable(String)
    /// A 401, 403 or 404. It will not clear until the config changes.
    case rejected(String)

    init(kind: String?, reason: String) {
        self = kind == "rejected" ? .rejected(reason) : .unreachable(reason)
    }

    var isOK: Bool { self == .ok }

    /// A few words for the menu, never the daemon's own error text.
    ///
    /// That text is a Go error carrying a full URL — `Get "https://redash…/api/
    /// data_sources": context deadline exceeded` — and an NSMenu item is one line
    /// that never wraps, so the menu widens to fit the longest thing in it. One
    /// error string is enough to stretch the whole menu across the screen. The
    /// original is in the log window, which is where a URL and a stack of wrapped
    /// causes are worth reading.
    var summary: String? {
        switch self {
        case .checking, .ok:
            return nil
        case .unreachable(let reason):
            return Self.reachabilityPhrase(for: reason)
        case .rejected(let reason):
            guard let status = Self.httpStatus(in: reason) else {
                return "Redash refused the request."
            }
            return "Redash answered with status \(status)."
        }
    }

    /// Matched on the text because Go's net and http errors arrive as prose. A
    /// phrase we do not recognise falls back to saying only what we know for
    /// certain, rather than pasting the error in and hoping it is short.
    private static func reachabilityPhrase(for reason: String) -> String {
        let text = reason.lowercased()

        if text.contains("context deadline exceeded") || text.contains("timeout") || text.contains("timed out") {
            return "The request timed out."
        }
        if text.contains("connection refused") {
            return "The connection was refused."
        }
        if text.contains("no such host") || text.contains("server misbehaving") {
            return "That host could not be found."
        }
        if text.contains("network is unreachable") || text.contains("no route to host") {
            return "The network is unreachable."
        }
        if text.contains("certificate") || text.contains("x509") || text.contains("tls") {
            return "The TLS certificate was rejected."
        }
        if let status = httpStatus(in: reason) {
            return "Redash answered with status \(status)."
        }
        return "Redash did not answer."
    }

    /// The daemon writes these as "… request failed (status 401)".
    private static func httpStatus(in reason: String) -> Int? {
        guard let marker = reason.range(of: "status ") else { return nil }
        let digits = reason[marker.upperBound...].prefix(while: \.isNumber)
        return digits.isEmpty ? nil : Int(digits)
    }

    var remedy: String? {
        switch self {
        case .checking, .ok:
            return nil
        case .unreachable:
            return "Check your VPN or network. Retrying automatically."
        case .rejected:
            return "Check the profile's API key and URL."
        }
    }
}

struct LogEvent: Identifiable, Equatable {
    enum Level: String, Comparable {
        case debug, info, warn, error, fatal

        private var rank: Int {
            switch self {
            case .debug: return 0
            case .info: return 1
            case .warn: return 2
            case .error: return 3
            case .fatal: return 4
            }
        }

        static func < (a: Level, b: Level) -> Bool { a.rank < b.rank }
    }

    let id = UUID()
    let time: Date
    let level: Level
    /// The daemon's machine-readable event name, when the line has one.
    let event: String?
    let message: String
    let fields: [String: String]
    /// True for a line that was not JSON: the Go runtime's crash output, or
    /// what the daemon prints before its logger exists. It belongs in the log,
    /// since it is usually the whole explanation of an exit, but it is not the
    /// daemon reporting on itself and the tracker weighs it differently.
    let isRaw: Bool

    init(time: Date, level: Level, event: String?, message: String, fields: [String: String], isRaw: Bool = false) {
        self.time = time
        self.level = level
        self.event = event
        self.message = message
        self.fields = fields
        self.isRaw = isRaw
    }

    /// A line that was not JSON. Error level, because the only things that write
    /// plain text to the daemon's stderr are the Go runtime, on a panic, and the
    /// daemon itself, on an error before its logger is set up.
    static func raw(line: String, now: Date) -> LogEvent {
        LogEvent(time: now, level: .error, event: nil, message: line, fields: [:], isRaw: true)
    }

    /// `sources` carries the entire data source list, which the menu renders and
    /// a one-line log column has no room for.
    private static let bulkFields: Set<String> = ["sources"]

    var fieldSummary: String {
        fields.filter { !Self.bulkFields.contains($0.key) }
            .sorted { $0.key < $1.key }
            .map { "\($0.key)=\($0.value)" }
            .joined(separator: " ")
    }

    /// One line of the daemon's `-log-format json` stream. Nil for anything that
    /// is not a JSON object, which the supervisor then keeps as `raw`.
    static func parse(line: Data) -> LogEvent? {
        guard
            let object = try? JSONSerialization.jsonObject(with: line),
            let record = object as? [String: Any]
        else { return nil }

        let message = record["msg"] as? String ?? ""
        let level = (record["level"] as? String).flatMap(Level.init(rawValue:)) ?? .info
        let time = (record["time"] as? String).flatMap(Self.timestampParser.date(from:)) ?? Date()
        let event = record["event"] as? String

        var fields: [String: String] = [:]
        for (key, value) in record where !["msg", "level", "time", "event"].contains(key) {
            fields[key] = String(describing: value)
        }

        return LogEvent(time: time, level: level, event: event, message: message, fields: fields)
    }

    private static let timestampParser: ISO8601DateFormatter = {
        let formatter = ISO8601DateFormatter()
        formatter.formatOptions = [.withInternetDateTime]
        return formatter
    }()
}
