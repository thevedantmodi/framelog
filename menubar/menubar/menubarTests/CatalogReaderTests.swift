import SQLite3
import XCTest

@testable import menubar

final class CatalogReaderTests: XCTestCase {
    // Creates a writable temp catalog.db with the PROTOCOL.md §1 schema.
    private func makeTempDB() -> (path: String, db: OpaquePointer) {
        let path = (NSTemporaryDirectory() as NSString)
            .appendingPathComponent("framelog_test_\(UUID().uuidString).db")
        var db: OpaquePointer?
        XCTAssertEqual(sqlite3_open_v2(path, &db, SQLITE_OPEN_READWRITE | SQLITE_OPEN_CREATE, nil), SQLITE_OK)
        let schema = """
            CREATE TABLE IF NOT EXISTS photos (
                hash              TEXT PRIMARY KEY,
                original_filename TEXT,
                imported_path     TEXT,
                camera_model      TEXT,
                capture_date      TEXT,
                import_timestamp  TEXT,
                gps_lat           REAL,
                gps_lon           REAL,
                status            TEXT DEFAULT 'raw'
            )
        """
        XCTAssertEqual(sqlite3_exec(db, schema, nil, nil, nil), SQLITE_OK)
        return (path, db!)
    }

    func testEmptyTable_photoCountZero_lastImportNil() {
        let (path, db) = makeTempDB()
        sqlite3_close(db)
        defer { try? FileManager.default.removeItem(atPath: path) }

        let snap = CatalogReader.read(dbPath: path)
        XCTAssertNotNil(snap, "read should succeed on a valid empty DB")
        XCTAssertEqual(snap?.photoCount, 0)
        XCTAssertNil(snap?.lastImport)
    }

    func testPopulatedTable_correctCountAndMostRecentTimestamp() {
        let (path, db) = makeTempDB()
        defer { try? FileManager.default.removeItem(atPath: path) }

        let rows: [(String, String)] = [
            ("hash1", "2026-01-01T10:00:00Z"),
            ("hash2", "2026-06-20T14:02:00Z"),  // most recent
            ("hash3", "2026-03-15T08:30:00Z"),
        ]
        for (hash, ts) in rows {
            let sql = "INSERT INTO photos (hash, import_timestamp) VALUES ('\(hash)', '\(ts)')"
            XCTAssertEqual(sqlite3_exec(db, sql, nil, nil, nil), SQLITE_OK)
        }
        sqlite3_close(db)

        let snap = CatalogReader.read(dbPath: path)
        XCTAssertEqual(snap?.photoCount, 3)
        XCTAssertEqual(snap?.lastImport, "2026-06-20T14:02:00Z")
    }

    func testNonexistentPath_returnsNil_noCrash() {
        let snap = CatalogReader.read(dbPath: "/nonexistent/no/such/catalog.db")
        XCTAssertNil(snap)
    }

    // Log-tail parsing — pure function, no file I/O needed.
    func testParseLogTail_returnsLastFiveLines() {
        let content = (1...10).map { "line \($0)" }.joined(separator: "\n")
        XCTAssertEqual(CatalogReader.parseLogTail(content, maxLines: 5),
                       ["line 6", "line 7", "line 8", "line 9", "line 10"])
    }

    func testParseLogTail_emptyContent_returnsEmpty() {
        XCTAssertEqual(CatalogReader.parseLogTail(""), [])
    }

    func testParseLogTail_fewerThanMaxLines_returnsAll() {
        let content = "line 1\nline 2\nline 3"
        XCTAssertEqual(CatalogReader.parseLogTail(content, maxLines: 5),
                       ["line 1", "line 2", "line 3"])
    }

    func testParseLogTail_skipsBlankLines() {
        let content = "a\n\nb\n\nc"
        XCTAssertEqual(CatalogReader.parseLogTail(content, maxLines: 5), ["a", "b", "c"])
    }
}
