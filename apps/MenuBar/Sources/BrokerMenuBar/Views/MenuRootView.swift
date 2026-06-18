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
        let visible = store.visibleBuilds
        if !visible.isEmpty {
            // Show the build list whenever we have builds — even mid-reconnect — so
            // the list stays stable and the header count always matches the rows.
            ScrollView {
                VStack(spacing: 0) {
                    ForEach(visible) { build in
                        BuildRowView(build: build)
                        Divider()
                    }
                }
            }
        } else if case .connected = store.connection {
            Text("No active builds")
                .foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, alignment: .center)
                .padding(.vertical, 18)
        } else {
            DisconnectedView()
        }
    }

    /// Daemon-lifecycle + cache-config actions. The broker runs as a LaunchAgent, so
    /// these manage it independently of the app (quitting the app never stops it).
    private var actions: some View {
        VStack(alignment: .leading, spacing: 8) {
            daemonToggle

            Divider()
            Text("SET UP A PROJECT")
                .font(.caption2)
                .foregroundStyle(.tertiary)

            labeledAction(
                title: "Speed Up a Project's Builds…",
                caption: "Pick a Bazel project folder. Configures it so all its git worktrees share one build cache — rebuilds reuse work instead of starting over.",
                id: "apply-cache-config-button"
            ) {
                if let dir = FolderPicker.chooseWorkspace(prompt: "Apply Config") {
                    chosenWorkspace = dir
                    store.applyCacheConfig(to: dir)
                }
            }

            labeledAction(
                title: "Queue a Project's Builds…",
                caption: "Optional. Routes a project's builds through the broker so it can queue/throttle them when your Mac is busy (installs a tools/bazel wrapper).",
                id: "install-wrapper-button"
            ) {
                let dir = chosenWorkspace ?? FolderPicker.chooseWorkspace(prompt: "Install Wrapper")
                if let dir {
                    chosenWorkspace = dir
                    store.installWrapper(in: dir)
                }
            }

            if let status = store.statusLine {
                StatusBanner(text: status)
                    .accessibilityIdentifier("status-line")
            }
        }
        .padding(8)
        .frame(maxWidth: .infinity, alignment: .leading)
    }

    /// A button with a plain-language caption underneath, so project-setup actions
    /// explain what they do without jargon.
    @ViewBuilder
    private func labeledAction(title: String,
                               caption: String,
                               id: String,
                               action: @escaping () -> Void) -> some View {
        VStack(alignment: .leading, spacing: 2) {
            Button(title, action: action)
                .accessibilityIdentifier(id)
            Text(caption)
                .font(.caption2)
                .foregroundStyle(.secondary)
                .fixedSize(horizontal: false, vertical: true)
        }
    }

    /// A single context-aware Start/Stop toggle (replaces separate Start/Restart/Stop):
    /// "Stop Broker" when running, "Start Broker" when not, disabled while starting.
    @ViewBuilder
    private var daemonToggle: some View {
        switch store.daemon {
        case .running:
            Button("Stop Broker") { store.stopBroker() }
                .help("Stop the broker daemon. This affects every tool, not just this app — builds stop being tracked and queued.")
                .accessibilityIdentifier("toggle-broker-button")
        case .starting:
            Button("Starting…") {}
                .disabled(true)
                .accessibilityIdentifier("toggle-broker-button")
        case .offline, .failed:
            Button("Start Broker") { store.startBroker() }
                .accessibilityIdentifier("toggle-broker-button")
        }
    }

    private var footer: some View {
        HStack {
            Spacer()
            Button("Quit") { NSApplication.shared.terminate(nil) }
                .accessibilityIdentifier("quit-button")
                .help("Quit this app. The broker keeps running in the background.")
        }
        .padding(8)
    }
}

/// Prominent feedback for project-setup / daemon actions: a spinner while a
/// trailing-"…" message is in progress, a green check on success, a red ✗ on
/// failure. Replaces the easy-to-miss dim caption.
struct StatusBanner: View {
    let text: String

    var body: some View {
        HStack(spacing: 6) {
            icon
            Text(text)
                .font(.callout)
                .fixedSize(horizontal: false, vertical: true)
            Spacer(minLength: 0)
        }
        .padding(8)
        .frame(maxWidth: .infinity, alignment: .leading)
        .background(tint.opacity(0.14), in: RoundedRectangle(cornerRadius: 6))
        .foregroundStyle(working ? .secondary : tint)
    }

    private var working: Bool { text.hasSuffix("…") }
    private var failed: Bool {
        let l = text.lowercased()
        return ["fail", "error", "did not", "unreach", "not found", "invalid"].contains { l.contains($0) }
    }

    @ViewBuilder private var icon: some View {
        if working {
            ProgressView().controlSize(.small)
        } else {
            Image(systemName: failed ? "xmark.circle.fill" : "checkmark.circle.fill")
        }
    }

    private var tint: Color { failed ? .red : .green }
}
