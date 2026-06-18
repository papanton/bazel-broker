import SwiftUI
import AppKit

/// The dropdown content: header, build list (or disconnected/empty state), footer.
/// Connection is tied to the menu lifetime via `.task`.
struct MenuRootView: View {
    @Environment(BrokerStore.self) private var store

    var body: some View {
        VStack(spacing: 0) {
            MenuHeaderView()
            Divider()

            content
                .frame(maxHeight: 360)

            Divider()
            footer
        }
        .task { store.start() }
    }

    @ViewBuilder
    private var content: some View {
        switch store.connection {
        case .connected:
            if store.builds.isEmpty {
                Text("No active builds")
                    .foregroundStyle(.secondary)
                    .frame(maxWidth: .infinity, alignment: .center)
                    .padding(.vertical, 18)
            } else {
                ScrollView {
                    VStack(spacing: 0) {
                        ForEach(store.sortedBuilds) { build in
                            BuildRowView(build: build)
                            Divider()
                        }
                    }
                }
            }
        case .connecting, .disconnected:
            DisconnectedView()
        }
    }

    private var footer: some View {
        HStack {
            Button("Reconnect") { store.reconnect() }
                .accessibilityIdentifier("reconnect-button")
            Spacer()
            Button("Quit") { NSApplication.shared.terminate(nil) }
                .accessibilityIdentifier("quit-button")
        }
        .padding(8)
    }
}
