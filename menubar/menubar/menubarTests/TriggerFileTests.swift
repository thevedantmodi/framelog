import XCTest

@testable import menubar

final class TriggerFileTests: XCTestCase {
    func testTouch_createsEmptyIngestTrigger() throws {
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }

        let trigger = dir.appendingPathComponent(".ingest_trigger")
        try touchTriggerFile(at: trigger)

        XCTAssertTrue(FileManager.default.fileExists(atPath: trigger.path))
        XCTAssertEqual(try Data(contentsOf: trigger).count, 0)
    }

    func testTouch_createsEmptyOutgestTrigger() throws {
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent(UUID().uuidString)
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        defer { try? FileManager.default.removeItem(at: dir) }

        let trigger = dir.appendingPathComponent(".outgest_trigger")
        try touchTriggerFile(at: trigger)

        XCTAssertTrue(FileManager.default.fileExists(atPath: trigger.path))
        XCTAssertEqual(try Data(contentsOf: trigger).count, 0)
    }
}
