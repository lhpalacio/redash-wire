import AppKit
import SwiftUI

/// The proxy's log stream, kept apart from the object the menu observes. The
/// menu is a real NSMenu that SwiftUI rebuilds on every published change, and a
/// TablePlus opening five connections writes five lines; with the log on the
/// supervisor, each one rebuilt the menu under the cursor.
@MainActor
final class LogStore: ObservableObject {
    private static let maxEvents = 5_000

    @Published private(set) var events: [LogEvent] = []

    /// Trimmed in batches: shifting all five thousand rows on every line, once
    /// the store was full, was most of the cost of a busy log.
    func append(_ event: LogEvent) {
        events.append(event)
        if events.count > Self.maxEvents + Self.maxEvents / 10 {
            events.removeFirst(events.count - Self.maxEvents)
        }
    }

    func clear() {
        events.removeAll()
    }
}

struct LogWindow: View {
    @ObservedObject var log: LogStore
    let diagnostics: @MainActor () -> String
    @State private var minimumLevel: LogEvent.Level = .debug
    @State private var searchText = ""
    /// Off lets you read back while lines keep arriving.
    @State private var follows = true

    private func visibleEvents() -> [LogEvent] {
        log.events.filter { event in
            guard event.level >= minimumLevel else { return false }
            guard !searchText.isEmpty else { return true }
            return event.message.localizedCaseInsensitiveContains(searchText)
                || event.fieldSummary.localizedCaseInsensitiveContains(searchText)
        }
    }

    /// `safeAreaInset` rather than a VStack: it reserves the toolbar's height so no
    /// row starts out hidden, while still letting the list scroll under the glass.
    /// The filter runs once per render; it used to run once per use.
    var body: some View {
        let visible = visibleEvents()
        eventList(visible)
            .safeAreaInset(edge: .top, spacing: 0) { toolbar(visible) }
            .frame(minWidth: 680, minHeight: 320)
    }

    private func toolbar(_ visibleEvents: [LogEvent]) -> some View {
        HStack(spacing: 12) {
            Picker("Level", selection: $minimumLevel) {
                Text("All").tag(LogEvent.Level.debug)
                Text("Info").tag(LogEvent.Level.info)
                Text("Warn").tag(LogEvent.Level.warn)
                Text("Error").tag(LogEvent.Level.error)
            }
            .pickerStyle(.menu)
            .fixedSize()

            TextField("Filter", text: $searchText)
                .textFieldStyle(.roundedBorder)
                .frame(maxWidth: 240)

            Spacer()

            Text("\(visibleEvents.count) of \(log.events.count)")
                .foregroundStyle(.secondary)
                .font(.callout)

            Toggle("Follow", isOn: $follows)
                .toggleStyle(.checkbox)

            Menu("More") {
                Button("Copy Shown Lines") { Clipboard.copy(visibleEvents.map(\.plainLine).joined(separator: "\n")) }
                    .disabled(visibleEvents.isEmpty)
                Button("Copy Diagnostics") { Clipboard.copy(diagnostics()) }
                Divider()
                Button("Show Log File in Finder") {
                    NSWorkspace.shared.activateFileViewerSelecting([LogFile.url])
                }
                Divider()
                Button("Clear") { log.clear() }
                    .disabled(log.events.isEmpty)
            }
            .fixedSize()
        }
        .padding(10)
        .glassBar()
    }

    @ViewBuilder
    private func eventList(_ visibleEvents: [LogEvent]) -> some View {
        if log.events.isEmpty {
            VStack {
                Spacer()
                Text("No log output yet.")
                    .foregroundStyle(.secondary)
                Text("Start the proxy to see events here.")
                    .foregroundStyle(.tertiary)
                    .font(.callout)
                Spacer()
            }
            .frame(maxWidth: .infinity)
        } else {
            ScrollViewReader { proxy in
                List(visibleEvents) { event in
                    row(event)
                        .id(event.id)
                }
                .listStyle(.plain)
                .font(.system(.body, design: .monospaced))
                // Keyed on the newest event, not the count: once the store is
                // at its cap the count never moves again, and the window stopped
                // following the log exactly when it had the most in it.
                .onChange(of: log.events.last?.id) { _ in
                    guard follows, let last = visibleEvents.last else { return }
                    proxy.scrollTo(last.id, anchor: .bottom)
                }
            }
        }
    }

    private func row(_ event: LogEvent) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(LogEvent.lineTimeFormatter.string(from: event.time))
                .foregroundStyle(.secondary)
            Text(event.level.rawValue.uppercased())
                .foregroundStyle(color(for: event.level))
                .frame(width: 52, alignment: .leading)
            VStack(alignment: .leading, spacing: 2) {
                Text(event.message)
                if !event.fieldSummary.isEmpty {
                    Text(event.fieldSummary)
                        .foregroundStyle(.secondary)
                        .font(.system(.callout, design: .monospaced))
                }
            }
            Spacer(minLength: 0)
        }
        .textSelection(.enabled)
        .padding(.vertical, 1)
    }

    private func color(for level: LogEvent.Level) -> Color {
        switch level {
        case .debug: return .secondary
        case .info: return .primary
        case .warn: return .orange
        case .error, .fatal: return .red
        }
    }
}
