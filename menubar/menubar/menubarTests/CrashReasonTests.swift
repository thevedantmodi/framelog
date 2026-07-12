import XCTest

@testable import menubar

final class CrashReasonTests: XCTestCase {
    func testCrashReason_nilContent() {
        XCTAssertNil(crashReason(logContent: nil))
    }

    func testCrashReason_emptyContent() {
        XCTAssertNil(crashReason(logContent: ""))
    }

    func testCrashReason_whitespaceOnlyContent() {
        XCTAssertNil(crashReason(logContent: "\n  \n\t\n"))
    }

    func testCrashReason_singleLine() {
        XCTAssertEqual(
            crashReason(logContent: "panic: runtime error: index out of range"),
            "panic: runtime error: index out of range")
    }

    func testCrashReason_multiLineReturnsLastNonEmptyLine() {
        let content = """
            panic: runtime error: index out of range [3] with length 3

            goroutine 1 [running]:
            main.mainRun()
            \t/Users/x/core/cmd/framelogd/main.go:42 +0x1a4
            """
        XCTAssertEqual(
            crashReason(logContent: content),
            "/Users/x/core/cmd/framelogd/main.go:42 +0x1a4")
    }

    func testCrashReason_trailingBlankLinesIgnored() {
        XCTAssertEqual(
            crashReason(logContent: "panic: boom\n\n\n"),
            "panic: boom")
    }
}
