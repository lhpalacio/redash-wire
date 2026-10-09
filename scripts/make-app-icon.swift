// Draws the app icon into macos/RedashWire/Assets.xcassets/AppIcon.appiconset.
//
//   swift scripts/make-app-icon.swift
//
// A database with a bolt on it: a wire into your data. The body follows
// Apple's macOS grid, an 824-point rounded square centred on a 1024 canvas
// with room for the shadow.
import AppKit

let root = URL(fileURLWithPath: CommandLine.arguments[0]).deletingLastPathComponent().deletingLastPathComponent()
let output = root.appendingPathComponent("macos/RedashWire/Assets.xcassets/AppIcon.appiconset")

func symbol(_ name: String, pointSize: CGFloat, color: NSColor) -> NSImage? {
    let config = NSImage.SymbolConfiguration(pointSize: pointSize, weight: .semibold)
        .applying(.init(paletteColors: [color]))
    return NSImage(systemSymbolName: name, accessibilityDescription: nil)?.withSymbolConfiguration(config)
}

func draw(_ image: NSImage, fitting box: CGFloat, centeredAt center: NSPoint) {
    let fit = min(box / image.size.width, box / image.size.height)
    let size = NSSize(width: image.size.width * fit, height: image.size.height * fit)
    image.draw(in: NSRect(x: center.x - size.width / 2, y: center.y - size.height / 2, width: size.width, height: size.height))
}

func drawMaster() -> NSImage {
    let size = NSSize(width: 1024, height: 1024)
    return NSImage(size: size, flipped: false) { _ in
        let body = NSRect(x: 100, y: 100, width: 824, height: 824)
        let shape = NSBezierPath(roundedRect: body, xRadius: 185, yRadius: 185)
        let slate = NSColor(calibratedRed: 0.07, green: 0.09, blue: 0.14, alpha: 1)
        let amber = NSColor(calibratedRed: 1.0, green: 0.71, blue: 0.16, alpha: 1)

        NSGraphicsContext.current?.saveGraphicsState()
        let shadow = NSShadow()
        shadow.shadowColor = NSColor.black.withAlphaComponent(0.3)
        shadow.shadowBlurRadius = 24
        shadow.shadowOffset = NSSize(width: 0, height: -12)
        shadow.set()
        slate.setFill()
        shape.fill()
        NSGraphicsContext.current?.restoreGraphicsState()

        NSGradient(colors: [
            NSColor(calibratedRed: 0.19, green: 0.24, blue: 0.35, alpha: 1),
            slate,
        ])?.draw(in: shape, angle: -90)

        if let cylinder = symbol("cylinder.split.1x2.fill", pointSize: 420, color: .white) {
            draw(cylinder, fitting: 470, centeredAt: NSPoint(x: body.midX - 40, y: body.midY + 30))
        }

        let badgeCenter = NSPoint(x: body.midX + 175, y: body.midY - 165)
        let badge = NSRect(x: badgeCenter.x - 135, y: badgeCenter.y - 135, width: 270, height: 270)
        slate.setFill()
        NSBezierPath(ovalIn: badge.insetBy(dx: -22, dy: -22)).fill()
        amber.setFill()
        NSBezierPath(ovalIn: badge).fill()
        if let bolt = symbol("bolt.fill", pointSize: 200, color: slate) {
            draw(bolt, fitting: 160, centeredAt: badgeCenter)
        }
        return true
    }
}

func png(_ image: NSImage, pixels: Int) -> Data {
    let rep = NSBitmapImageRep(
        bitmapDataPlanes: nil, pixelsWide: pixels, pixelsHigh: pixels,
        bitsPerSample: 8, samplesPerPixel: 4, hasAlpha: true, isPlanar: false,
        colorSpaceName: .deviceRGB, bytesPerRow: 0, bitsPerPixel: 0
    )!
    rep.size = NSSize(width: pixels, height: pixels)
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = NSGraphicsContext(bitmapImageRep: rep)
    NSGraphicsContext.current?.imageInterpolation = .high
    image.draw(in: NSRect(x: 0, y: 0, width: pixels, height: pixels))
    NSGraphicsContext.restoreGraphicsState()
    return rep.representation(using: .png, properties: [:])!
}

let master = drawMaster()
try FileManager.default.createDirectory(at: output, withIntermediateDirectories: true)

var images: [[String: String]] = []
for points in [16, 32, 128, 256, 512] {
    for scale in [1, 2] {
        let name = "icon_\(points)x\(points)\(scale == 2 ? "@2x" : "").png"
        try png(master, pixels: points * scale).write(to: output.appendingPathComponent(name))
        images.append(["size": "\(points)x\(points)", "idiom": "mac", "filename": name, "scale": "\(scale)x"])
    }
}

let contents: [String: Any] = ["images": images, "info": ["version": 1, "author": "xcode"]]
try JSONSerialization.data(withJSONObject: contents, options: [.prettyPrinted, .sortedKeys])
    .write(to: output.appendingPathComponent("Contents.json"))
try #"{"info":{"author":"xcode","version":1}}"#.data(using: .utf8)!
    .write(to: output.deletingLastPathComponent().appendingPathComponent("Contents.json"))
print("wrote \(images.count) images to \(output.path)")
