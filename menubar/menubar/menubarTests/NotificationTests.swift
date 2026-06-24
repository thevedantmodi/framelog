import XCTest

@testable import menubar

final class NotificationTests: XCTestCase {
    func testDelta_multiplePhotos() {
        XCTAssertEqual(importDeltaMessage(oldCount: 5, newCount: 8), "Imported 3 new photos")
    }
    func testDelta_singlePhoto() {
        XCTAssertEqual(importDeltaMessage(oldCount: 0, newCount: 1), "Imported 1 new photo")
    }
    func testDelta_noNewPhotos() {
        XCTAssertEqual(importDeltaMessage(oldCount: 5, newCount: 5), "Imported 0 new photos")
    }
    func testDelta_negativeGuardedToZero() {
        XCTAssertEqual(importDeltaMessage(oldCount: 10, newCount: 5), "Imported 0 new photos")
    }
}
