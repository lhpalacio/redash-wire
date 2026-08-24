import SwiftUI

struct LogWindow: View {
    @ObservedObject var supervisor: ProxySupervisor
    @State private var minimumLevel: LogEvent.Level = .debug
    @State private var searchText = ""

    private var visibleEvents: [LogEvent] {
        supervisor.events.filter { event in
            guard event.level >= minimumLevel else { return false }
            guard !searchText.isEmpty else { return true }
            return event.message.localizedCaseInsensitiveContains(searchText)
                || event.fieldSummary.localizedCaseInsensitiveContains(searchText)
        }
    }

    /// `safeAreaInset` rather than a VStack: it reserves the toolbar's height so no
    /// row starts out hidden, while still letting the list scroll under the glass.
    var body: some View {
        eventList
            .safeAreaInset(edge: .top, spacing: 0) { toolbar }
            .frame(minWidth: 620, minHeight: 320)
    }

    private var toolbar: some View {
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

            Text("\(visibleEvents.count) of \(supervisor.events.count)")
                .foregroundStyle(.secondary)
                .font(.callout)

            Button("Copy") { Clipboard.copy(plainText) }
                .disabled(visibleEvents.isEmpty)
            Button("Clear") { supervisor.clearLog() }
                .disabled(supervisor.events.isEmpty)
        }
        .padding(10)
        .glassBar()
    }

    @ViewBuilder
    private var eventList: some View {
        if supervisor.events.isEmpty {
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
                .onChange(of: supervisor.events.count) { _ in
                    guard let last = visibleEvents.last else { return }
                    withAnimation { proxy.scrollTo(last.id, anchor: .bottom) }
                }
            }
        }
    }

    private func row(_ event: LogEvent) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 8) {
            Text(Self.timeFormatter.string(from: event.time))
                .foregroundStyle(.secondary)
            Text(event.level.rawValue.uppercased())
                .foregroundStyle(color(for: event.level))
                .frame(width: 52, alignment: .leading)
            VStack(alignment: .leading, spacing: 2) {
                Text(event.message)
                if !event.fields.isEmpty {
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

    private var plainText: String {
        visibleEvents.map { event in
            let stamp = Self.timeFormatter.string(from: event.time)
            let fields = event.fields.isEmpty ? "" : "  " + event.fieldSummary
            return "\(stamp) \(event.level.rawValue.uppercased()) \(event.message)\(fields)"
        }
        .joined(separator: "\n")
    }

    private static let timeFormatter: DateFormatter = {
        let formatter = DateFormatter()
        formatter.dateFormat = "HH:mm:ss"
        return formatter
    }()
}
