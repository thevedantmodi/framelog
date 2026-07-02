import XCTest

@testable import menubar

final class CapabilityDisplayTests: XCTestCase {
    // MARK: backupStatusLine

    func testBackup_rcloneMissingWinsOverEverything() {
        XCTAssertEqual(
            backupStatusLine(rcloneAvailable: false, configured: true, mounted: true),
            "Backup off — rclone not installed")
    }

    func testBackup_notConfigured() {
        XCTAssertEqual(
            backupStatusLine(rcloneAvailable: true, configured: false, mounted: false),
            "Backup not set up")
    }

    func testBackup_configuredAndMounted() {
        XCTAssertEqual(
            backupStatusLine(rcloneAvailable: true, configured: true, mounted: true),
            "Backup drive connected")
    }

    func testBackup_configuredButUnplugged() {
        XCTAssertEqual(
            backupStatusLine(rcloneAvailable: true, configured: true, mounted: false),
            "Backup drive not connected")
    }

    // MARK: capabilityWarnings

    func testWarnings_allCapabilitiesPresent() {
        let caps: [String: Any] = [
            "sd_card_watch": true, "backup": true,
            "ac_power_gate": true, "lightroom_check": true,
        ]
        XCTAssertEqual(capabilityWarnings(caps), [])
    }

    func testWarnings_missingBinariesEachProduceOneWarning() {
        let caps: [String: Any] = [
            "sd_card_watch": false, "backup": false,
            "ac_power_gate": false, "lightroom_check": false,
        ]
        let warnings = capabilityWarnings(caps)
        // backup is deliberately absent — it has the dedicated backup line.
        XCTAssertEqual(warnings, [
            "SD card detection off — diskutil missing",
            "AC-power check off — pmset missing",
            "Lightroom check off — pgrep missing",
        ])
    }

    func testWarnings_nilOrEmptyCapabilitiesStayQuiet() {
        // Older daemons without the capabilities field must not warn.
        XCTAssertEqual(capabilityWarnings(nil), [])
        XCTAssertEqual(capabilityWarnings([:]), [])
    }
}
