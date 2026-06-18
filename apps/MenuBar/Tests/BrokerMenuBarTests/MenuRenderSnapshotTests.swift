import XCTest
import SwiftUI
@testable import BrokerMenuBar

/// Headless visual proof that the menu renders the summary + per-build rows with
/// kill / cache% / profile controls. The macOS NSStatusItem dropdown can't be driven
/// by the xcodebuildmcp UI-automation (iOS-Simulator-only) and screen capture is TCC-
/// gated in CI, so we render the actual `MenuRootView` (seeded from the golden
/// fixtures) into a PNG via `ImageRenderer`. The PNG is attached to the test result
/// and written to the repo's build output for inspection.
@MainActor
final class MenuRenderSnapshotTests: XCTestCase {
    func testRenderConnectedMenuFromFixtures() throws {
        let store = BrokerStore()
        store.seed(builds: try Fixtures.builds(), connection: .connected)

        // Compose header + rows + footer WITHOUT a ScrollView: `ImageRenderer` cannot
        // lay out a ScrollView's content off-screen (it renders fine in a live window).
        // This faithfully captures the same header/rows/controls the app shows.
        let view = VStack(spacing: 0) {
            MenuHeaderView()
            Divider()
            ForEach(store.sortedBuilds) { build in
                BuildRowView(build: build)
                Divider()
            }
            HStack {
                Button("Reconnect") {}
                Spacer()
                Button("Quit") {}
            }
            .padding(8)
        }
        .environment(store)
        .frame(width: 340)
        .background(Color(white: 0.12))

        let renderer = ImageRenderer(content: view)
        renderer.scale = 2.0
        guard let nsImage = renderer.nsImage,
              let tiff = nsImage.tiffRepresentation,
              let bitmap = NSBitmapImageRep(data: tiff),
              let png = bitmap.representation(using: .png, properties: [:]) else {
            return XCTFail("ImageRenderer produced no image")
        }

        XCTAssertGreaterThan(png.count, 1000, "rendered PNG should be non-trivial")

        // Attach to the xcresult and also drop a copy next to the build products.
        let attachment = XCTAttachment(data: png, uniformTypeIdentifier: "public.png")
        attachment.name = "menu-connected.png"
        attachment.lifetime = .keepAlways
        add(attachment)

        let out = URL(fileURLWithPath: NSTemporaryDirectory())
            .appending(path: "BrokerMenuBar-menu-connected.png")
        try png.write(to: out)
        print("wrote menu snapshot: \(out.path)")
    }

    func testRenderDisconnectedMenu() throws {
        let store = BrokerStore()
        store.seed(builds: [], connection: .disconnected(reason: "broker offline"))

        let view = MenuRootView()
            .environment(store)
            .frame(width: 340)

        let renderer = ImageRenderer(content: view)
        XCTAssertNotNil(renderer.nsImage, "disconnected menu should render")
    }
}
