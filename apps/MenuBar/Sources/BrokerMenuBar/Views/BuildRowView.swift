import SwiftUI

/// One build: worktree name, targets, live elapsed, optional cache% badge, a state
/// badge, an "open profile" action when `profile_url` is present, and a kill button.
struct BuildRowView: View {
    let build: Build
    @Environment(BrokerStore.self) private var store

    var body: some View {
        HStack(alignment: .top, spacing: 8) {
            Image(systemName: "shippingbox")
                .foregroundStyle(.secondary)
                .font(.headline)
                .help("Bazel build in this worktree")
            VStack(alignment: .leading, spacing: 2) {
                Text(build.displayName)
                    .font(.headline)
                    .accessibilityIdentifier("build-name-\(build.invocationID)")
                // Secondary line: the targets being built, or — for a discovered build
                // with no known targets — the worktree path so the name has context.
                if !build.targets.isEmpty {
                    Text(build.targets.joined(separator: " "))
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                } else {
                    Text(build.worktree)
                        .font(.caption)
                        .foregroundStyle(.secondary)
                        .lineLimit(1)
                        .truncationMode(.middle)
                }
                metaRow
                if build.isActive {
                    // Bazel doesn't stream a completion %, so show an indeterminate
                    // activity bar — the row clearly reads as "in progress" alongside
                    // the live-ticking elapsed time.
                    ProgressView()
                        .progressViewStyle(.linear)
                        .controlSize(.small)
                        .frame(maxWidth: .infinity)
                        .accessibilityIdentifier("progress-\(build.invocationID)")
                }
            }
            Spacer(minLength: 4)
            actions
        }
        .padding(8)
        .opacity(build.isActive ? 1.0 : 0.55)
        .help(build.worktree)
    }

    private var metaRow: some View {
        // 1 Hz tick so live elapsed advances without server traffic.
        TimelineView(.periodic(from: .now, by: 1)) { context in
            HStack(spacing: 8) {
                Text(ElapsedFormatter.string(for: build, now: context.date))
                    .monospacedDigit()
                if let ratio = build.cacheHitRatio {
                    CacheBadge(ratio: ratio)
                        .accessibilityIdentifier("cache-\(build.invocationID)")
                }
                StateBadge(state: build.state)
            }
            .font(.caption2)
        }
    }

    private var actions: some View {
        VStack(spacing: 6) {
            Button {
                store.kill(build.invocationID)
            } label: {
                Image(systemName: "stop.circle")
            }
            .buttonStyle(.borderless)
            .help("Kill build")
            .accessibilityIdentifier("kill-\(build.invocationID)")
            .disabled(!build.isActive)

            if build.profileURL != nil {
                Button {
                    PerfettoOpener.open(build)
                } label: {
                    Image(systemName: "chart.bar.xaxis")
                }
                .buttonStyle(.borderless)
                .help("Open profile in Perfetto")
                .accessibilityIdentifier("profile-\(build.invocationID)")
            }
        }
    }
}

/// "cache 87%" pill, tinted by hit ratio.
struct CacheBadge: View {
    let ratio: Double

    var body: some View {
        Text("cache \(Int((ratio * 100).rounded()))%")
            .padding(.horizontal, 5)
            .padding(.vertical, 1)
            .background(tint.opacity(0.18), in: Capsule())
            .foregroundStyle(tint)
    }

    private var tint: Color {
        switch ratio {
        case 0.8...: return .green
        case 0.4..<0.8: return .orange
        default: return .red
        }
    }
}

/// State pill mirroring the wire state value.
struct StateBadge: View {
    let state: BuildState

    var body: some View {
        Text(state.rawValue)
            .padding(.horizontal, 5)
            .padding(.vertical, 1)
            .background(tint.opacity(0.18), in: Capsule())
            .foregroundStyle(tint)
    }

    private var tint: Color {
        switch state {
        case .running: return .blue
        case .queued: return .gray
        case .finished: return .green
        case .failed: return .red
        case .killed: return .orange
        case .gone, .unknown: return .gray
        }
    }
}
