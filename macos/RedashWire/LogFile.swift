import Foundation

/// Everything the daemon writes, kept in ~/Library/Logs/RedashWire so it
/// outlives the app: the log window is memory only, and a panic's trace used
/// to be lost on quit. One previous file is kept when it rotates.
final class LogFile: @unchecked Sendable {
    static let shared = LogFile()

    static let directory = FileManager.default.homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Logs/RedashWire", isDirectory: true)
    static let url = directory.appendingPathComponent("redash-wire.log")

    private static let rotateAt: UInt64 = 5 * 1024 * 1024

    private let queue = DispatchQueue(label: "redash-wire.log-file")
    private var handle: FileHandle?

    func append(_ line: Data) {
        queue.async { [self] in
            guard let handle = openHandle() else { return }
            try? handle.write(contentsOf: line)
            try? handle.write(contentsOf: Data([UInt8(ascii: "\n")]))
            if let size = try? handle.offset(), size >= Self.rotateAt {
                rotate()
            }
        }
    }

    /// A line of the app's own, so a session in the file starts somewhere.
    func mark(_ text: String) {
        let stamp = ISO8601DateFormatter().string(from: Date())
        append(Data("--- \(stamp) \(text) ---".utf8))
    }

    private func openHandle() -> FileHandle? {
        if let handle { return handle }
        let manager = FileManager.default
        try? manager.createDirectory(at: Self.directory, withIntermediateDirectories: true)
        if !manager.fileExists(atPath: Self.url.path) {
            manager.createFile(atPath: Self.url.path, contents: nil, attributes: [.posixPermissions: 0o600])
        }
        handle = try? FileHandle(forWritingTo: Self.url)
        _ = try? handle?.seekToEnd()
        return handle
    }

    private func rotate() {
        try? handle?.close()
        handle = nil
        let previous = Self.url.appendingPathExtension("1")
        try? FileManager.default.removeItem(at: previous)
        try? FileManager.default.moveItem(at: Self.url, to: previous)
    }
}
