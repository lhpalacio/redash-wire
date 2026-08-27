import Foundation

/// Every call passes `-config` explicitly. The app does not control its working
/// directory, and redash-wire falls back to `./config.yaml` without the flag, so a
/// stray file there could point the proxy at another Redash.
struct WireCLI {
    let binaryURL: URL
    let configPath: String


    func config() async throws -> ConfigPayload {
        try await runJSON(["config", "-json", "-config", configPath])
    }

    func dataSources(profile: String) async throws -> [DataSource] {
        try await runJSON(["datasources", "-json", "-config", configPath, "-profile", profile])
    }

    /// The key goes over stdin, never argv, which `ps` exposes.
    func initialize(redashURL: String, profile: String, apiKey: String) async throws -> InitResult {
        try await runJSON(
            ["init", "-json", "-config", configPath, "-url", redashURL, "-profile", profile],
            stdin: Data(apiKey.utf8)
        )
    }

    /// `-exit-on-stdin-eof` stops a force-quit from leaving an orphan on the ports.
    /// It only works while the supervisor holds the child's stdin pipe open.
    ///
    /// `-wait-for-redash` makes an unreachable Redash a state to show rather than
    /// a reason to exit, so launching at login before the VPN is up leaves a proxy
    /// that recovers by itself. The bare CLI still fails fast without it.
    func serveArguments(profile: String) -> [String] {
        [
            "-config", configPath,
            "-profile", profile,
            "-log-format=json",
            "-exit-on-stdin-eof",
            "-wait-for-redash",
        ]
    }


    private struct ProcessResult {
        let stdout: Data
        let stderr: Data
        let status: Int32
    }

    private func runJSON<T: Decodable>(_ arguments: [String], stdin: Data? = nil) async throws -> T {
        let result = try await run(arguments, stdin: stdin)

        guard result.status == 0 else {
            throw Self.wireError(from: result)
        }

        do {
            return try JSONDecoder().decode(T.self, from: result.stdout)
        } catch {
            let command = arguments.first ?? "redash-wire"
            throw WireError(
                code: .unknown,
                message: "could not read the output of `\(command)`: \(error.localizedDescription)"
            )
        }
    }

    private func run(_ arguments: [String], stdin: Data?) async throws -> ProcessResult {
        let process = Process()
        process.executableURL = binaryURL
        process.arguments = arguments

        let outPipe = Pipe()
        let errPipe = Pipe()
        let inPipe = Pipe()
        process.standardOutput = outPipe
        process.standardError = errPipe
        process.standardInput = inPipe

        do {
            try process.run()
        } catch {
            throw WireError(
                code: .unknown,
                message: "could not run \(binaryURL.path): \(error.localizedDescription)"
            )
        }

        // Close so a command reading stdin sees EOF instead of blocking.
        if let stdin {
            try? inPipe.fileHandleForWriting.write(contentsOf: stdin)
        }
        try? inPipe.fileHandleForWriting.close()

        // Concurrently: waiting on one while the other fills its 64KB buffer deadlocks.
        async let out = Self.readToEnd(outPipe.fileHandleForReading)
        async let err = Self.readToEnd(errPipe.fileHandleForReading)
        let (stdoutData, stderrData) = await (out, err)

        process.waitUntilExit()
        return ProcessResult(stdout: stdoutData, stderr: stderrData, status: process.terminationStatus)
    }

    private static func readToEnd(_ handle: FileHandle) async -> Data {
        await withCheckedContinuation { continuation in
            DispatchQueue.global(qos: .userInitiated).async {
                continuation.resume(returning: handle.readDataToEndOfFile())
            }
        }
    }

    /// A usage mistake exits before any JSON is written, so fall back to stderr
    /// instead of inventing a code.
    private static func wireError(from result: ProcessResult) -> WireError {
        if let payload = try? JSONDecoder().decode(CLIErrorPayload.self, from: result.stdout) {
            return WireError(code: payload.error.code, message: payload.error.message)
        }

        let text = String(decoding: result.stderr, as: UTF8.self)
            .trimmingCharacters(in: .whitespacesAndNewlines)
        return WireError(
            code: .unknown,
            message: text.isEmpty ? "redash-wire exited with status \(result.status)" : text
        )
    }
}

extension WireCLI {
    static var defaultConfigPath: String {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent(".redash-wire/config.yaml")
            .path
    }

    /// The bundled binary, so the app and the proxy cannot disagree on the contract.
    /// REDASH_WIRE_BINARY overrides it during development.
    static func standard(configPath: String = defaultConfigPath) -> WireCLI {
        WireCLI(binaryURL: bundledBinaryURL(), configPath: configPath)
    }

    static func bundledBinaryURL() -> URL {
        if let override = ProcessInfo.processInfo.environment["REDASH_WIRE_BINARY"] {
            return URL(fileURLWithPath: override)
        }
        if let bundled = Bundle.main.url(forResource: "redash-wire", withExtension: nil) {
            return bundled
        }
        return Bundle.main.bundleURL
            .appendingPathComponent("Contents/Resources/redash-wire")
    }
}
