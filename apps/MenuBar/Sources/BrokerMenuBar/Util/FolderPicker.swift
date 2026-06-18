import AppKit

/// Wraps `NSOpenPanel` for picking a Bazel workspace directory. The panel itself
/// cannot be driven headlessly (no first-class `xcodebuildmcp` macOS UI automation);
/// the logic it feeds — `CacheConfigApplier` — is unit-tested directly instead.
enum FolderPicker {
    /// Present a directory chooser; returns the chosen URL or nil if cancelled.
    @MainActor
    static func chooseWorkspace(prompt: String = "Choose") -> URL? {
        let panel = NSOpenPanel()
        panel.canChooseDirectories = true
        panel.canChooseFiles = false
        panel.allowsMultipleSelection = false
        panel.prompt = prompt
        panel.message = "Choose a Bazel workspace / worktree"
        // Bring the agent (LSUIElement) to the front so the panel is interactive.
        NSApp.activate(ignoringOtherApps: true)
        return panel.runModal() == .OK ? panel.url : nil
    }
}
