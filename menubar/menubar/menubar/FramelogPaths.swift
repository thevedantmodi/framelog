import Foundation
import Darwin

enum FramelogPaths {
    static var photosDir: URL {
        FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Photos")
    }
    static var catalogDB: URL    { photosDir.appendingPathComponent("catalog.db") }
    static var framelogLog: URL  { photosDir.appendingPathComponent("framelog.log") }
    static var ingestTrigger: URL  { photosDir.appendingPathComponent(".ingest_trigger") }
    static var outgestTrigger: URL { photosDir.appendingPathComponent(".outgest_trigger") }

    // Must match config.SocketPath in the Go core.
    static var socket: URL {
        FileManager.default.homeDirectoryForCurrentUser
            .appendingPathComponent("Library/Application Support/Framelog/framelog.sock")
    }

    // framelogdBinary: checked in priority order.
    // 1. Bundled in Contents/MacOS/ next to the Swift executable (distribution path).
    // 2. Common manual locations for the dev workflow.
    static var framelogdBinary: URL? {
        let candidates: [URL] = [
            Bundle.main.executableURL?
                .deletingLastPathComponent()
                .appendingPathComponent("framelogd"),
            URL(fileURLWithPath: "/usr/local/bin/framelogd"),
            FileManager.default.homeDirectoryForCurrentUser
                .appendingPathComponent(".local/bin/framelogd"),
        ].compactMap { $0 }
        return candidates.first { FileManager.default.isExecutableFile(atPath: $0.path) }
    }
}
