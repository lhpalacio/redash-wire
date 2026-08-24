import SwiftUI

extension View {
    /// A control strip that content scrolls underneath.
    ///
    /// Liquid Glass needs macOS 26 and the deployment target is 13.0, so anything
    /// older keeps the flat bar and divider. Both paths reserve the same space.
    @ViewBuilder
    func glassBar() -> some View {
        if #available(macOS 26.0, *) {
            glassEffect(.regular, in: .rect)
        } else {
            background(.bar)
                .overlay(alignment: .bottom) { Divider() }
        }
    }

    /// A floating card, and a no-op below macOS 26. Nothing sits behind this window
    /// for a material to shine through, so the older path leaves it plain.
    @ViewBuilder
    func glassPanel(cornerRadius: CGFloat) -> some View {
        if #available(macOS 26.0, *) {
            glassEffect(.regular, in: .rect(cornerRadius: cornerRadius))
        } else {
            self
        }
    }
}
