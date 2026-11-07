import XCTest

@testable import menubar

final class ReportingTests: XCTestCase {
    private func card(
        safe: Bool = true, backup: String = "verified", problems: [String] = [],
        total: Int = 412, verified: Int = 412
    ) -> CardInfo {
        CardInfo(
            id: 1, volumeName: "Untitled", finishedAt: "", safeToFormat: safe,
            filesTotal: total, bytesTotal: 0, filesVerified: verified,
            filesProblem: total - verified, filesNew: 400, filesExisting: 12,
            fileTypes: "ARW 206 · JPG 206", cameraModels: "ILCE-7CM2",
            backupState: backup, problems: problems, reportPath: "/r.txt")
    }

    // MARK: Parsing

    func testCardInfo_parsesStatusJSON() throws {
        let json: [String: Any] = [
            "id": 7, "volume_name": "Untitled", "safe_to_format": false,
            "files_total": 3, "bytes_total": NSNumber(value: Int64(64_000_000_000)),
            "files_verified": 2, "files_problem": 1, "backup_state": "incomplete",
            "problems": ["1 file is not on the backup drive yet"], "report_path": "",
        ]
        let c = try XCTUnwrap(CardInfo(json: json))
        XCTAssertEqual(c.id, 7)
        XCTAssertFalse(c.safeToFormat)
        XCTAssertEqual(c.bytesTotal, 64_000_000_000)
        XCTAssertEqual(c.problems.count, 1)
        XCTAssertNil(c.reportPath, "empty report_path should be nil")
    }

    func testCardInfo_missingOrOldDaemonIsNil() {
        XCTAssertNil(CardInfo(json: nil))
        XCTAssertNil(CardInfo(json: ["volume_name": "x"]), "no id → not a report")
    }

    func testActivityInfo_parses() throws {
        let a = try XCTUnwrap(
            ActivityInfo(json: [
                "phase": "copying", "volume": "Untitled", "done": 12, "total": 340,
                "current_file": "DSC00012.ARW",
            ]))
        XCTAssertTrue(a.isBusy)
        XCTAssertEqual(a.currentFile, "DSC00012.ARW")
        XCTAssertNil(ActivityInfo(json: nil))
    }

    // MARK: Activity

    func testActivityLine() {
        func line(_ phase: String, _ done: Int, _ total: Int, _ vol: String? = "Untitled") -> String? {
            activityDisplayLine(
                ActivityInfo(phase: phase, volume: vol, done: done, total: total, currentFile: nil))
        }
        XCTAssertNil(line("idle", 0, 0))
        XCTAssertEqual(line("copying", 12, 340), "Copying from Untitled… 12 of 340")
        XCTAssertEqual(line("importing", 3, 10, nil), "Importing… 3 of 10")
        XCTAssertEqual(line("backing_up", 5, 0, nil), "Backing up… 5 files copied")
        XCTAssertEqual(line("backing_up", 0, 0, nil), "Backing up…")
        XCTAssertEqual(line("verifying", 1, 2), "Verifying Untitled… 1 of 2")
    }

    // MARK: Card report

    func testVerdictLine() {
        XCTAssertEqual(cardVerdictLine(card()), "✓ Untitled: safe to format")
        XCTAssertEqual(cardVerdictLine(card(safe: false)), "✗ Untitled: don't format yet")
    }

    func testDetailLines_includeWhatWasCopiedAndProblems() {
        let lines = cardDetailLines(card(safe: false, backup: "incomplete", problems: ["2 files are not on the backup drive yet"], verified: 410))
        XCTAssertTrue(lines.contains("Camera: ILCE-7CM2"))
        XCTAssertTrue(lines.contains("400 newly imported · 12 already in library"))
        XCTAssertTrue(lines.contains("410 of 412 verified byte-for-byte"))
        XCTAssertTrue(lines.contains("Some files are missing from the backup drive"))
        XCTAssertTrue(lines.contains("⚠ 2 files are not on the backup drive yet"))
        XCTAssertTrue(lines[0].hasPrefix("412 files"))
        XCTAssertTrue(lines[0].contains("ARW 206 · JPG 206"))
    }

    func testCardNotification() {
        let ok = cardNotification(card())
        XCTAssertEqual(ok.title, "Untitled is safe to format")
        XCTAssertEqual(ok.body, "All 412 files verified in your library and on the backup drive.")

        let libOnly = cardNotification(card(backup: "not_configured"))
        XCTAssertEqual(libOnly.body, "All 412 files verified in your library.")

        let bad = cardNotification(card(safe: false, problems: ["first", "second", "third"]))
        XCTAssertEqual(bad.title, "Don't format Untitled yet")
        XCTAssertEqual(bad.body, "first (+2 more)")
    }

    // MARK: Backup

    private func backup(_ outcome: String, pending: Bool, error: String = "") -> BackupInfo {
        BackupInfo(
            pending: pending, lastOutcome: outcome, lastError: error,
            lastRunAt: "2026-11-07T10:00:00Z", lastSuccessAt: "", filesFailed: 2)
    }

    func testBackupProblemLine() {
        XCTAssertNil(backupProblemLine(backup("failed", pending: true), configured: false))
        XCTAssertEqual(
            backupProblemLine(backup("failed", pending: true, error: "Backup drive is full"), configured: true),
            "⚠ Backup failed: Backup drive is full")
        XCTAssertEqual(
            backupProblemLine(backup("drive_missing", pending: true), configured: true),
            "⚠ Backup waiting — drive wasn't connected")
        XCTAssertNil(backupProblemLine(backup("ok", pending: false), configured: true))
        XCTAssertEqual(
            backupProblemLine(backup("ok", pending: true), configured: true),
            "⚠ Backup pending — not all photos are on the drive yet")
    }

    func testBackupFailureNotification() {
        XCTAssertNil(backupFailureNotification(backup("ok", pending: false)))
        let n = backupFailureNotification(backup("failed", pending: true, error: "Backup drive is full"))
        XCTAssertEqual(n?.title, "Backup failed")
        XCTAssertEqual(n?.body, "Backup drive is full (2 files not backed up)")
    }

    // MARK: Command results

    func testVerifyResultMessage() {
        XCTAssertEqual(verifyResultMessage(nil), "Core didn't respond")
        XCTAssertEqual(
            verifyResultMessage(["ok": false, "error": "no_card"]), "No camera card is mounted")
        XCTAssertEqual(
            verifyResultMessage([
                "ok": true, "card": ["id": 1, "volume_name": "Untitled", "safe_to_format": true],
            ]), "✓ Untitled: safe to format")
    }

    func testBackupNowMessage() {
        XCTAssertEqual(
            backupNowMessage(["ok": true, "outcome": "ok", "files_copied": 1]),
            "Backup complete — 1 file copied")
        XCTAssertEqual(
            backupNowMessage(["ok": true, "outcome": "failed", "backup_error": "Backup drive is full"]),
            "Backup failed: Backup drive is full")
        XCTAssertEqual(
            backupNowMessage(["ok": false, "error": "backup_not_configured"]), "No backup drive is set")
    }
}
