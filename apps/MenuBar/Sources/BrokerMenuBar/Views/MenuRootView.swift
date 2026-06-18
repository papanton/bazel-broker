import SwiftUI
import AppKit

/// The dropdown content: header, build list (or disconnected/empty state), footer.
/// Connection is tied to the menu lifetime via `.task`.
struct MenuRootView: View {
    @Environment(BrokerStore.self) private var store
    /// The workspace last chosen via "Apply Cache Config…", so "Install build wrapper"
    /// can target the same directory without re-prompting.
    @State private var chosenWorkspace: URL?

    var body: some View {
        VStack(spacing: 0) {
            MenuHeaderView()
            Divider()

            content
                .frame(maxHeight: 360)

            Divider()
            actions
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

    /// Daemon-lifecycle + cache-config actions. The broker runs as a LaunchAgent, so
    /// these manage it independently of the app (quitting the app never stops it).
    private var actions: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Button("Start Broker") { store.startBroker() }
                    .accessibilityIdentifier("start-broker-button")
                Button("Restart Broker") { store.restartBroker() }
                    .accessibilityIdentifier("restart-broker-button")
                Button("Stop Broker") { store.stopBroker() }
                    .accessibilityIdentifier("stop-broker-button")
            }
            Button("Apply Cache Config to a Folder…") {
                if let dir = FolderPicker.chooseWorkspace(prompt: "Apply Config") {
                    chosenWorkspace = dir
                    store.applyCacheConfig(to: dir)
                }
            }
            .accessibilityIdentifier("apply-cache-config-button")

            Button("Install build wrapper…") {
                let dir = chosenWorkspace ?? FolderPicker.chooseWorkspace(prompt: "Install Wrapper")
                if let dir {
                    chosenWorkspace = dir
                    store.installWrapper(in: dir)
                }
            }
            .accessibilityIdentifier("install-wrapper-button")
            .help("Optional: drop tools/bazel into the workspace for block-before-build admission")

            if let status = store.statusLine {
                Text(status)
                    .font(.caption2)
                    .foregroundStyle(.secondary)
                    .lineLimit(2)
                    .accessibilityIdentifier("status-line")
            }
        }
        .padding(8)
        .frame(maxWidth: .infinity, alignment: .leading)
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
