import SwiftUI

@main
struct menubarApp: App {
    @StateObject private var status = FramelogStatus()

    var body: some Scene {
        MenuBarExtra("Framelog", systemImage: "photo.stack") {
            ContentView()
                .environmentObject(status)
        }
        .menuBarExtraStyle(.menu)
    }
}
