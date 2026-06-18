import SwiftUI

/// "N building, M queued" summary plus a connection-status dot.
struct MenuHeaderView: View {
    @Environment(BrokerStore.self) private var store

    var body: some View {
        HStack {
            Image(systemName: "hammer.fill")
                .foregroundStyle(.secondary)
            Text(summaryText)
                .font(.headline)
                .accessibilityIdentifier("summary-label")
            Spacer()
            connectionDot
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
}
