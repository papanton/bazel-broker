import Foundation
import AppKit

/// Opens a build's profile. `profile_url` is a fully-formed URL that E4 populates
/// (per the frozen contract — clients just `open` it); the app does not parse or
/// serve the profile itself.
enum PerfettoOpener {
    static func open(_ build: Build) {
        guard let raw = build.profileURL, let url = URL(string: raw) else { return }
        NSWorkspace.shared.open(url)
    }
}
