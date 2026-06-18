import SwiftUI

/// "N building, M queued" summary plus a connection-status dot.
struct MenuHeaderView: View {
    @Environment(BrokerStore.self) private var store

    var body: some View {
        VStack(spacing: 4) {
            HStack {
                Image(systemName: "hammer.fill")
                    .foregroundStyle(.secondary)
                Text(summaryText)
                    .font(.headline)
                    .accessibilityIdentifier("summary-label")
                Spacer()
                connectionDot
            }
            HStack {
                daemonDot
                Text(store.daemon.label)
                    .font(.caption)
                    .foregroundStyle(.secondary)
                    .accessibilityIdentifier("daemon-label")
                Spacer()
            }
        }
        .padding(8)
    }

    private var summaryText: String {
        let s = store.summary
        return "\(s.building) building, \(s.queued) queued"
    }

    @ViewBuilder
    private var connectionDot: some View {
        switch store.connection {
        case .connected:
            Circle().fill(.green).frame(width: 8, height: 8)
                .help("Connected")
        case .connecting:
            Circle().fill(.yellow).frame(width: 8, height: 8)
                .help("Connecting")
        case .disconnected:
            Circle().fill(.red).frame(width: 8, height: 8)
                .help("Disconnected")
        }
    }

    @ViewBuilder
    private var daemonDot: some View {
        switch store.daemon {
        case .running:
            Circle().fill(.green).frame(width: 6, height: 6)
        case .starting:
            Circle().fill(.yellow).frame(width: 6, height: 6)
        case .offline:
            Circle().fill(.gray).frame(width: 6, height: 6)
        case .failed:
            Circle().fill(.red).frame(width: 6, height: 6)
        }
    }
}
