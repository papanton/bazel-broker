import SwiftUI

/// Shown when the broker is unreachable or not yet connected. Resilient by design:
/// the store keeps retrying with backoff; "Reconnect" in the footer forces an
/// immediate retry.
struct DisconnectedView: View {
    @Environment(BrokerStore.self) private var store

    var body: some View {
        VStack(spacing: 6) {
            Image(systemName: "bolt.horizontal.circle")
                .font(.title2)
                .foregroundStyle(.secondary)
            Text(store.connection.disconnectedTitle)
                .font(.headline)
            Text(store.connection.disconnectedDetail)
                .font(.caption)
                .foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 18)
        .padding(.horizontal, 12)
        .accessibilityIdentifier("disconnected-view")
    }
}
