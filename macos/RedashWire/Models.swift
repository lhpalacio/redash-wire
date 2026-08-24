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
        case valid
        case error
    }

    /// The supervisor waits for this many `listening` events before it calls the
    /// proxy ready.
    var enabledListenerCount: Int {
        [postgresListenAddr, mysqlListenAddr].filter { !$0.isEmpty }.count
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
    let message: String
    let fields: [String: String]

    var fieldSummary: String {
        fields.sorted { $0.key < $1.key }
            .map { "\($0.key)=\($0.value)" }
            .joined(separator: " ")
    }
}
