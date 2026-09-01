// swift-tools-version: 5.9

// The app is built by the Xcode project next to this file; this package exists
// so the parts of it that own no process and no window can be tested with
// `swift test`. It compiles RedashWire/Core a second time, on its own, which is
// also what proves those files depend on nothing in the rest of the app.
import PackageDescription

let package = Package(
    name: "RedashWireCore",
    platforms: [.macOS(.v13)],
    targets: [
        .target(name: "RedashWireCore", path: "RedashWire/Core"),
        .testTarget(name: "RedashWireCoreTests", dependencies: ["RedashWireCore"], path: "Tests/RedashWireCoreTests"),
    ]
)
