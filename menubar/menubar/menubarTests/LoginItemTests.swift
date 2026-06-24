import ServiceManagement
import XCTest

@testable import menubar

final class LoginItemTests: XCTestCase {
    // Label text
    func testLabel_notRegistered()    { XCTAssertEqual(loginItemLabelString(status: .notRegistered), "Launch at Login") }
    func testLabel_enabled()          { XCTAssertEqual(loginItemLabelString(status: .enabled), "Launch at Login") }
    func testLabel_requiresApproval() { XCTAssertEqual(loginItemLabelString(status: .requiresApproval), "Launch at Login (check System Settings)") }
    func testLabel_notFound()         { XCTAssertEqual(loginItemLabelString(status: .notFound), "Launch at Login") }

    // Toggle checked state
    func testChecked_enabled()          { XCTAssertTrue(loginItemIsChecked(status: .enabled)) }
    func testChecked_requiresApproval() { XCTAssertTrue(loginItemIsChecked(status: .requiresApproval)) }
    func testChecked_notRegistered()    { XCTAssertFalse(loginItemIsChecked(status: .notRegistered)) }
    func testChecked_notFound()         { XCTAssertFalse(loginItemIsChecked(status: .notFound)) }

    // Toggle interactivity
    func testInteractive_notFound()         { XCTAssertFalse(loginItemIsInteractive(status: .notFound)) }
    func testInteractive_notRegistered()    { XCTAssertTrue(loginItemIsInteractive(status: .notRegistered)) }
    func testInteractive_enabled()          { XCTAssertTrue(loginItemIsInteractive(status: .enabled)) }
    func testInteractive_requiresApproval() { XCTAssertTrue(loginItemIsInteractive(status: .requiresApproval)) }
}
