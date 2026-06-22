# PROTOCOL.md — Framelog core ↔ frontend contract

This is the only thing the Go core and the Swift menu bar need to agree on. Either side
should be buildable from this document alone, without reading the other side's source.

Status: **draft** — fill in as each ticket lands; treat changes to this file as part of
the same PR that changes the contract (see roadmap Definition of Done).

---

## 1. `catalog.db` schema (frozen contract — core writes, frontend reads)

```sql
CREATE TABLE IF NOT EXISTS photos (
    hash               TEXT PRIMARY KEY,
    original_filename  TEXT,
    imported_path       TEXT,
    camera_model        TEXT,
    capture_date        TEXT,
    import_timestamp    TEXT,
    gps_lat              REAL,
    gps_lon              REAL,
    status               TEXT DEFAULT 'raw'  -- raw | culled | edited | published
);
```

- Core opens with `PRAGMA journal_mode=WAL;` and a `busy_timeout`. (FL-004)
- Frontend opens **read-only** (`?mode=ro` DSN or equivalent). Never writes to this file.
- `status` is either fully wired up by FL-103/FL-201/FL-202, or removed from this schema
  entirely — it does not ship half-used again.

## 2. v1 IPC — trigger file (FL-301)

- Path: `~/Photos/.ingest_trigger`
- Frontend (or `on_sd_mount` logic) creates an empty file at this path to request an
  ingest run.
- Core polls for its existence, deletes it, then runs ingest. No payload — this is a
  pure signal, not a command channel.

## 3. v2 IPC — Unix domain socket (FL-302)

- Path: `~/Library/Application Support/Framelog/framelog.sock`
- Transport: line-delimited JSON, one request per line, one response per line.

**Requests:**

```json
{"command": "ingest_now"}
{"command": "outgest_now"}
{"command": "status"}
```

**Responses:**

```json
{"ok": true, "imported": 3, "skipped": 1, "failed": 0}
{"ok": true, "moved": 2, "skipped": 0, "failed": 0}
{"ok": true, "running": true, "photo_count": 4213, "last_import": "2026-06-20T14:02:00Z", "backup_drive_mounted": true}
{"ok": false, "error": "core unreachable"}
```

- `status` must always answer, even mid-ingest — frontend uses this to render
  "core unreachable" cleanly per FL-604.

## 4. Log line format

```
2026-06-22 14:03:11 [INGEST] Done: 3 imported, 1 skipped, 0 failed
```

`TIMESTAMP [PREFIX] message`, one logger, always flushed. Frontend tails this file for
the log viewer (FL-403/FL-406). No bare `print()`-equivalent anywhere in the core — the
Python version's `ingest.py` vs. `outgest.py` split doesn't get a sequel here.

## 5. File layout on disk

```
~/Photos/
├── inbox/              ← landing zone, cleared after import
├── originals/YYYY/MM/DD/<ts>_<hash8>.ext + .xmp
├── processed/YYYY/MM/   ← Lightroom exports, organized by outgest
├── catalog.db
└── framelog.log

~/Library/LaunchAgents/
└── com.framelog.core.plist   ← installed by the core itself (FL-303)

~/Library/Application Support/Framelog/
└── framelog.sock              ← v2 IPC socket (FL-302)
```

## 6. Open questions (resolve before Phase 2)

- [ ] `crs:` XMP namespace — in scope for v1 or not? (FL-105)
- [ ] `gitops`: shell out to `git` CLI, or `go-git`? (FL-106)
- [ ] Backup target: `rclone copy` of `originals/` post-ingest — confirmed in FL-207,
      replaces the old (disabled) pre-ingest raw-SD-card sync.
