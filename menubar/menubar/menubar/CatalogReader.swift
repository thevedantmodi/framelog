import Foundation
import SQLite3

struct CatalogSnapshot {
    let photoCount: Int
    let lastImport: String?
}

enum CatalogReader {
    // Opens catalog.db read-only (PROTOCOL.md §1 — frontend never writes this file).
    // Returns nil if the file doesn't exist or can't be opened — callers use nil to
    // distinguish "core not running" from "core running but zero photos".
    static func read(dbPath: String) -> CatalogSnapshot? {
        var db: OpaquePointer?
        // immutable=1 skips WAL/shm files entirely. Without it, SQLITE_OPEN_READONLY
        // fails to create the -shm file that WAL mode requires. Safe here because
        // we open/close on every poll tick and never write.
        let uri = "file:\(dbPath)?immutable=1"
        let rc = sqlite3_open_v2(uri, &db, SQLITE_OPEN_READONLY | SQLITE_OPEN_URI, nil)
        guard rc == SQLITE_OK, let db else {
            sqlite3_close(db)
            return nil
        }
        defer { sqlite3_close(db) }
        return CatalogSnapshot(
            photoCount: queryPhotoCount(db: db),
            lastImport: queryLastImport(db: db)
        )
    }

    private static func queryPhotoCount(db: OpaquePointer) -> Int {
        var stmt: OpaquePointer?
        guard sqlite3_prepare_v2(db, "SELECT COUNT(*) FROM photos", -1, &stmt, nil) == SQLITE_OK else { return 0 }
        defer { sqlite3_finalize(stmt) }
        return sqlite3_step(stmt) == SQLITE_ROW ? Int(sqlite3_column_int64(stmt, 0)) : 0
    }

    private static func queryLastImport(db: OpaquePointer) -> String? {
        var stmt: OpaquePointer?
        let sql = "SELECT import_timestamp FROM photos ORDER BY import_timestamp DESC LIMIT 1"
        guard sqlite3_prepare_v2(db, sql, -1, &stmt, nil) == SQLITE_OK else { return nil }
        defer { sqlite3_finalize(stmt) }
        guard sqlite3_step(stmt) == SQLITE_ROW,
              let cStr = sqlite3_column_text(stmt, 0) else { return nil }
        return String(cString: cStr)
    }

    // Reads the last maxBytes of the log file and returns the last maxLines non-empty lines.
    // Consistent with PROTOCOL.md §4's poll model — no FSEvents tailing for v1.
    static func logTail(logPath: String, maxBytes: Int = 4096, maxLines: Int = 5) -> [String] {
        guard let fh = FileHandle(forReadingAtPath: logPath) else { return [] }
        defer { try? fh.close() }
        let size = (try? fh.seekToEnd()) ?? 0
        let offset = size > UInt64(maxBytes) ? size - UInt64(maxBytes) : 0
        try? fh.seek(toOffset: offset)
        guard let data = try? fh.readToEnd(),
              let content = String(data: data, encoding: .utf8) else { return [] }
        return parseLogTail(content, maxLines: maxLines)
    }

    // Pure function — accepts a raw string, returns last maxLines non-empty lines.
    // Split out so tests can exercise the parsing logic against an in-memory string.
    static func parseLogTail(_ content: String, maxLines: Int = 5) -> [String] {
        content.components(separatedBy: "\n")
            .map { $0.trimmingCharacters(in: .whitespaces) }
            .filter { !$0.isEmpty }
            .suffix(maxLines)
            .map { String($0) }
    }
}
