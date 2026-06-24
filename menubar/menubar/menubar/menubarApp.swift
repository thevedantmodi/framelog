import SwiftUI

@main
struct menubarApp: App {
    @StateObject private var status = FramelogStatus()

    var body: some Scene {
        MenuBarExtra("Framelog", image: "MenuBarIcon") {
            ContentView()
                .environmentObject(status)
        }
        .menuBarExtraStyle(.menu)
    }
}
