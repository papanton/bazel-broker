import SwiftUI
import AppKit

/// Owns the single store and connects it eagerly at launch (D-E8-3) so the menu-bar
/// glyph reflects live state and the dropdown is fresh on first open.
/// `applicationDidFinishLaunching` is guaranteed to run on the main actor, which is
/// the reliable place to kick off the @MainActor store's connect loop.
@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate {
    let store = BrokerStore()

    func applicationDidFinishLaunching(_ notification: Notification) {
        store.start()
    }
}

/// The menu-bar agent. A `MenuBarExtra` with `.window` style so the dropdown is a
/// real SwiftUI hierarchy (kill buttons, live-updating rows). `LSUIElement=YES` in
/// Info.plist makes it a pure agent — no Dock icon, no main window.
@main
struct BrokerMenuBarApp: App {
    @NSApplicationDelegateAdaptor(AppDelegate.self) private var delegate

    var body: some Scene {
        MenuBarExtra {
            MenuRootView()
                .environment(delegate.store)
                .frame(width: 340)
        } label: {
            MenuBarLabel(summary: delegate.store.summary, connection: delegate.store.connection)
        }
        .menuBarExtraStyle(.window)
    }
}

/// The glanceable menu-bar label — derived purely from the store. Shows the count of
/// running builds when connected, a muted glyph otherwise.
struct MenuBarLabel: View {
    let summary: BuildSummary
    let connection: ConnectionState

    var body: some View {
        switch connection {
        case .connected:
            if summary.building > 0 {
                Image(systemName: "hammer.fill")
                Text("\(summary.building)")
            } else {
                Image(systemName: "hammer")
            }
        case .connecting:
            Image(systemName: "hammer")
        case .disconnected:
            Image(systemName: "hammer.badge.exclamationmark")
        }
    }
}
