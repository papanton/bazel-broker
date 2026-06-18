import SwiftUI
import Greeting
import Analytics

@main
struct BrokerDemoApp: App {
    var body: some Scene {
        WindowGroup {
            ContentView()
        }
    }
}

struct ContentView: View {
    private let numbers = [1, 2, 3, 4, 5]

    var body: some View {
        VStack(spacing: 12) {
            Text(Greeter.greet("Bazel Broker"))
                .font(.headline)
            Text("Sum: \(Analytics.summarize(numbers))")
                .font(.subheadline)
        }
        .padding()
    }
}
