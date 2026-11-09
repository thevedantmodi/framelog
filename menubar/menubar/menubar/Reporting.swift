import Foundation

// Models + pure display functions for the richer status fields
// (PROTOCOL.md §3: "activity", "backup", "last_card") and the verify_card /
// backup_now commands. Everything here is side-effect free so it can be
// unit-tested without a running core — FramelogStatus only wires it up.

// MARK: - Models

// Live progress of the card → library → backup pipeline.
struct ActivityInfo: Equatable {
    var phase: String  // idle | copying | importing | backing_up | verifying
    var volume: String?
    var done: Int
    var total: Int  // 0 = unknown
    var currentFile: String?

    var isBusy: Bool { phase != "idle" }
}

extension ActivityInfo {
    init?(json: [String: Any]?) {
        guard let json, let phase = json["phase"] as? String else { return nil }
        self.init(
            phase: phase,
            volume: nonEmpty(json["volume"] as? String),
            done: json["done"] as? Int ?? 0,
            total: json["total"] as? Int ?? 0,
            currentFile: nonEmpty(json["current_file"] as? String))
    }
}

// Outcome of the most recent backup attempt.
struct BackupInfo: Equatable {
    var pending: Bool
    var lastOutcome: String  // ok | failed | drive_missing | rclone_missing | "" (never ran)
    var lastError: String
    var lastRunAt: String
    var lastSuccessAt: String
    var filesFailed: Int
}

extension BackupInfo {
    init?(json: [String: Any]?) {
        guard let json else { return nil }
        self.init(
            pending: json["pending"] as? Bool ?? false,
            lastOutcome: json["last_outcome"] as? String ?? "",
            lastError: json["last_error"] as? String ?? "",
            lastRunAt: json["last_run_at"] as? String ?? "",
            lastSuccessAt: json["last_success_at"] as? String ?? "",
            filesFailed: json["files_failed"] as? Int ?? 0)
    }
}

// One end-to-end card verification (the "can I format this card?" answer).
struct CardInfo: Equatable {
    var id: Int
    var volumeName: String
    var finishedAt: String
    var safeToFormat: Bool
    var filesTotal: Int
    var bytesTotal: Int64
    var filesVerified: Int
    var filesProblem: Int
    var filesNew: Int
    var filesExisting: Int
    var fileTypes: String
    var cameraModels: String
    var backupState: String  // verified | not_configured | drive_missing | incomplete
    var problems: [String]
    var reportPath: String?
}

extension CardInfo {
    init?(json: [String: Any]?) {
        guard let json, let id = json["id"] as? Int else { return nil }
        self.init(
            id: id,
            volumeName: json["volume_name"] as? String ?? "Card",
            finishedAt: json["finished_at"] as? String ?? "",
            safeToFormat: json["safe_to_format"] as? Bool ?? false,
            filesTotal: json["files_total"] as? Int ?? 0,
            bytesTotal: (json["bytes_total"] as? NSNumber)?.int64Value ?? 0,
            filesVerified: json["files_verified"] as? Int ?? 0,
            filesProblem: json["files_problem"] as? Int ?? 0,
            filesNew: json["files_new"] as? Int ?? 0,
            filesExisting: json["files_existing"] as? Int ?? 0,
            fileTypes: json["file_types"] as? String ?? "",
            cameraModels: json["camera_models"] as? String ?? "",
            backupState: json["backup_state"] as? String ?? "",
            problems: json["problems"] as? [String] ?? [],
            reportPath: nonEmpty(json["report_path"] as? String))
    }
}

// Quick "what's new on this card" preview, present while a card is mounted.
struct PreviewInfo: Equatable {
    var volumeName: String
    var checkedAt: String
    var filesTotal: Int
    var filesNew: Int
    var bytesNew: Int64
    var filesInLibrary: Int
    var filesIgnored: Int
    var filesUnhandled: Int
    var newFileTypes: String
}

extension PreviewInfo {
    init?(json: [String: Any]?) {
        guard let json, let name = json["volume_name"] as? String else { return nil }
        self.init(
            volumeName: name,
            checkedAt: json["checked_at"] as? String ?? "",
            filesTotal: json["files_total"] as? Int ?? 0,
            filesNew: json["files_new"] as? Int ?? 0,
            bytesNew: (json["bytes_new"] as? NSNumber)?.int64Value ?? 0,
            filesInLibrary: json["files_in_library"] as? Int ?? 0,
            filesIgnored: json["files_ignored"] as? Int ?? 0,
            filesUnhandled: json["files_unhandled"] as? Int ?? 0,
            newFileTypes: json["new_file_types"] as? String ?? "")
    }
}

// One row of the File Types menu.
struct FileTypeInfo: Equatable {
    var ext: String  // ".arw"
    var label: String
    var enabled: Bool
    var custom: Bool
}

extension FileTypeInfo {
    init?(json: [String: Any]) {
        guard let ext = json["ext"] as? String else { return nil }
        self.init(
            ext: ext,
            label: json["label"] as? String ?? "",
            enabled: json["enabled"] as? Bool ?? false,
            custom: json["custom"] as? Bool ?? false)
    }

    static func list(json: Any?) -> [FileTypeInfo] {
        (json as? [[String: Any]] ?? []).compactMap { FileTypeInfo(json: $0) }
    }
}

// State of a long-running menu action (Verify Card / Retry Backup).
enum ActionState: Equatable {
    case idle
    case running
    case finished(String)

    var isRunning: Bool { self == .running }
}

private func nonEmpty(_ s: String?) -> String? {
    guard let s, !s.isEmpty else { return nil }
    return s
}

// MARK: - Formatting helpers

func plural(_ n: Int, _ one: String, _ many: String) -> String {
    "\(n) \(n == 1 ? one : many)"
}

// "3 hours ago" for an RFC 3339 timestamp; nil if unparseable/empty.
func relativeTime(iso: String, now: Date = Date()) -> String? {
    guard let date = ISO8601DateFormatter().date(from: iso) else { return nil }
    let fmt = RelativeDateTimeFormatter()
    fmt.unitsStyle = .full
    return fmt.localizedString(for: date, relativeTo: now)
}

func byteString(_ bytes: Int64) -> String {
    ByteCountFormatter.string(fromByteCount: bytes, countStyle: .file)
}

// MARK: - Activity

// One line describing what the core is doing, or nil when idle.
func activityDisplayLine(_ a: ActivityInfo) -> String? {
    guard a.isBusy else { return nil }
    let progress: String
    if a.total > 0 {
        progress = " \(a.done) of \(a.total)"
    } else if a.done > 0 {
        progress = " \(a.done) so far"
    } else {
        progress = ""
    }
    let from = a.volume.map { " \($0)" } ?? ""
    switch a.phase {
    case "copying": return "Copying from\(from)…\(progress)"
    case "importing": return "Importing…\(progress)"
    case "backing_up":
        return a.done > 0 ? "Backing up… \(plural(a.done, "file", "files")) copied" : "Backing up…"
    case "verifying": return "Verifying\(from)…\(progress)"
    case "scanning": return "Checking\(from) for new files…"
    default: return "Working…\(progress)"
    }
}

// MARK: - Card report

// Headline for the last card: the answer to "can I format it?".
func cardVerdictLine(_ c: CardInfo) -> String {
    c.safeToFormat
        ? "✓ \(c.volumeName): safe to format"
        : "✗ \(c.volumeName): don't format yet"
}

// Where the card's files are verified to exist.
func cardBackupLine(_ c: CardInfo) -> String {
    switch c.backupState {
    case "verified": return "Verified in library and on backup drive"
    case "not_configured": return "Verified in library only — no backup drive set up"
    case "drive_missing": return "Backup drive wasn't connected during the check"
    case "incomplete": return "Some files are missing from the backup drive"
    default: return "Library check complete"
    }
}

// Detail lines for the card submenu: what was copied, where it is, what's wrong.
func cardDetailLines(_ c: CardInfo, now: Date = Date()) -> [String] {
    var lines: [String] = []
    var summary = plural(c.filesTotal, "file", "files")
    if c.bytesTotal > 0 { summary += " (\(byteString(c.bytesTotal)))" }
    if !c.fileTypes.isEmpty { summary += " · \(c.fileTypes)" }
    lines.append(summary)
    if !c.cameraModels.isEmpty { lines.append("Camera: \(c.cameraModels)") }
    if c.filesNew > 0 || c.filesExisting > 0 {
        lines.append("\(c.filesNew) newly imported · \(c.filesExisting) already in library")
    }
    lines.append("\(c.filesVerified) of \(c.filesTotal) verified byte-for-byte")
    lines.append(cardBackupLine(c))
    lines += c.problems.map { "⚠ \($0)" }
    if let when = relativeTime(iso: c.finishedAt, now: now) {
        lines.append("Checked \(when)")
    }
    return lines
}

// Notification for a newly finished card report.
func cardNotification(_ c: CardInfo) -> (title: String, body: String) {
    if c.safeToFormat {
        let where_ =
            c.backupState == "verified" ? "in your library and on the backup drive" : "in your library"
        let body =
            c.filesTotal == 0
            ? "No photos or videos were on the card."
            : "All \(plural(c.filesTotal, "file", "files")) verified \(where_)."
        return ("\(c.volumeName) is safe to format", body)
    }
    var body = c.problems.first ?? "\(plural(c.filesProblem, "file needs", "files need")) attention."
    if c.problems.count > 1 { body += " (+\(c.problems.count - 1) more)" }
    return ("Don't format \(c.volumeName) yet", body)
}

// MARK: - Backup

// A warning line for the backup section, or nil when there's nothing to
// warn about. rclone_missing is left to the capability line.
func backupProblemLine(_ b: BackupInfo, configured: Bool) -> String? {
    guard configured else { return nil }
    switch b.lastOutcome {
    case "failed":
        let detail = b.lastError.isEmpty ? "see log" : b.lastError
        return "⚠ Backup failed: \(detail)"
    case "drive_missing" where b.pending:
        return "⚠ Backup waiting — drive wasn't connected"
    default:
        return b.pending ? "⚠ Backup pending — not all photos are on the drive yet" : nil
    }
}

// "· last backup 2 hours ago", appended to the existing backup line.
func lastBackupSuffix(_ b: BackupInfo, now: Date = Date()) -> String {
    guard let when = relativeTime(iso: b.lastSuccessAt, now: now) else { return "" }
    return " · last backup \(when)"
}

// Notification for a backup attempt that just failed, or nil.
func backupFailureNotification(_ b: BackupInfo) -> (title: String, body: String)? {
    guard b.lastOutcome == "failed" else { return nil }
    var body = b.lastError.isEmpty ? "See the log for details." : b.lastError
    if b.filesFailed > 0 {
        body += " (\(plural(b.filesFailed, "file", "files")) not backed up)"
    }
    return ("Backup failed", body)
}

// MARK: - Command results

// Message for a verify_card response (nil json = core unreachable).
func verifyResultMessage(_ json: [String: Any]?) -> String {
    guard let json else { return "Core didn't respond" }
    if json["ok"] as? Bool == true, let card = CardInfo(json: json["card"] as? [String: Any]) {
        return cardVerdictLine(card)
    }
    switch json["error"] as? String {
    case "no_card": return "No camera card is mounted"
    case "card_busy": return "The card is already being checked"
    case "verify_unavailable": return "Card checks need diskutil and rclone"
    case let code?: return "Verify failed (\(code))"
    case nil: return "Verify failed"
    }
}

// Message for a backup_now response (nil json = core unreachable).
func backupNowMessage(_ json: [String: Any]?) -> String {
    guard let json else { return "Core didn't respond" }
    if json["ok"] as? Bool == true {
        let copied = json["files_copied"] as? Int ?? 0
        switch json["outcome"] as? String {
        case "ok": return "Backup complete — \(plural(copied, "file", "files")) copied"
        case "drive_missing": return "Backup drive isn't connected"
        case "rclone_missing": return "rclone isn't installed"
        default:
            let err = json["backup_error"] as? String ?? ""
            return "Backup failed: \(err.isEmpty ? "see log" : err)"
        }
    }
    switch json["error"] as? String {
    case "backup_not_configured": return "No backup drive is set"
    case "ingest_already_running": return "Busy importing — try again shortly"
    case "ingest_paused": return "Framelog is paused"
    case let code?: return "Backup failed (\(code))"
    case nil: return "Backup failed"
    }
}

// MARK: - Card preview

// "Untitled: 312 new (8.1 GB) · 1,040 already imported"
func previewLine(_ p: PreviewInfo) -> String {
    if p.filesTotal == 0 {
        return "\(p.volumeName): no photos or videos"
    }
    if p.filesNew == 0 {
        return "\(p.volumeName): all \(plural(p.filesTotal, "file", "files")) already imported"
    }
    var line = "\(p.volumeName): \(p.filesNew) new"
    if p.bytesNew > 0 { line += " (\(byteString(p.bytesNew)))" }
    if p.filesInLibrary > 0 { line += " · \(p.filesInLibrary) already imported" }
    return line
}

// Extra lines under the preview: what's new, and anything skipped.
func previewDetailLines(_ p: PreviewInfo) -> [String] {
    var lines: [String] = []
    if p.filesNew > 0, !p.newFileTypes.isEmpty { lines.append("New: \(p.newFileTypes)") }
    if p.filesIgnored > 0 {
        lines.append("\(plural(p.filesIgnored, "file", "files")) skipped (switched off in File Types)")
    }
    if p.filesUnhandled > 0 {
        lines.append("⚠ \(plural(p.filesUnhandled, "file", "files")) of a type Framelog doesn't know")
    }
    return lines
}

func previewNotification(_ p: PreviewInfo) -> (title: String, body: String) {
    if p.filesTotal == 0 {
        return ("\(p.volumeName) inserted", "No photos or videos on this card.")
    }
    if p.filesNew == 0 {
        return (
            "\(p.volumeName): nothing new",
            "All \(plural(p.filesTotal, "file is", "files are")) already in your library. Checking them now."
        )
    }
    var body = "\(plural(p.filesNew, "new file", "new files")) to import"
    if p.bytesNew > 0 { body += " (\(byteString(p.bytesNew)))" }
    if p.filesInLibrary > 0 { body += ", \(p.filesInLibrary) already in your library" }
    return ("\(p.volumeName) inserted", body + ".")
}

// The preview belongs to the card run in progress; once that run's report
// exists (finished after the preview was taken) the verdict replaces it.
func shouldShowPreview(_ p: PreviewInfo, lastCard: CardInfo?) -> Bool {
    guard let lastCard else { return true }
    let fmt = ISO8601DateFormatter()
    guard let checked = fmt.date(from: p.checkedAt),
        let finished = fmt.date(from: lastCard.finishedAt)
    else { return true }
    return checked > finished
}

// MARK: - File types

// ".ARW — Sony RAW"
func fileTypeMenuLabel(_ t: FileTypeInfo) -> String {
    let ext = t.ext.uppercased()
    return t.custom ? "\(ext) — added by you" : "\(ext) — \(t.label)"
}

// The enabled list after toggling ext, or nil when that would switch off
// the last type (the core rejects an empty list).
func importTypesAfterToggling(_ types: [FileTypeInfo], ext: String) -> [String]? {
    var enabled = types.filter(\.enabled).map(\.ext)
    if let i = enabled.firstIndex(of: ext) {
        enabled.remove(at: i)
    } else {
        enabled.append(ext)
    }
    return enabled.isEmpty ? nil : enabled.sorted()
}

// The enabled list with a user-typed extension added ("insv", ".INSV"),
// or nil if the input isn't a plausible extension.
func importTypesAdding(_ types: [FileTypeInfo], typed: String) -> [String]? {
    var ext = typed.trimmingCharacters(in: .whitespacesAndNewlines).lowercased()
    if !ext.hasPrefix(".") { ext = "." + ext }
    let body = ext.dropFirst()
    guard (1...10).contains(body.count), body.allSatisfy({ $0.isASCII && ($0.isLetter || $0.isNumber) })
    else { return nil }
    var enabled = Set(types.filter(\.enabled).map(\.ext))
    enabled.insert(ext)
    return enabled.sorted()
}

// JSON for set_import_types; nil types = reset to defaults.
func importTypesPayload(_ types: [String]?) -> String {
    var obj: [String: Any] = ["command": "set_import_types"]
    if let types {
        obj["types"] = types
    } else {
        obj["types"] = NSNull()
    }
    guard let data = try? JSONSerialization.data(withJSONObject: obj, options: [.sortedKeys]),
        let str = String(data: data, encoding: .utf8)
    else { return "{\"command\":\"set_import_types\"}" }
    return str
}

func setImportTypesMessage(_ json: [String: Any]?) -> String? {
    guard let json else { return "Core didn't respond" }
    if json["ok"] as? Bool == true { return nil }
    switch json["error"] as? String {
    case "invalid_file_type": return "That file type can't be imported"
    case let code?: return "Couldn't change file types (\(code))"
    case nil: return "Couldn't change file types"
    }
}
