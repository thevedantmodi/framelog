# PROTOCOL.md — Framelog core ↔ frontend contract

The only thing the Go core and the Swift menu bar need to agree on. Either side is
buildable from this document alone without reading the other side's source. Any change
to the contract must be reflected here in the same commit — this is part of the
Definition of Done for every ticket.

**Status: implemented and tested.** All sections below describe the current shipped
behaviour, not a design draft.

---

## 1. `catalog.db` schema (core writes, frontend reads read-only)

```sql
CREATE TABLE IF NOT EXISTS photos (
    hash              TEXT PRIMARY KEY,
    original_filename TEXT,
    imported_path     TEXT,
    camera_model      TEXT,
    capture_date      TEXT,
    import_timestamp  TEXT,     -- ISO 8601, UTC
    gps_lat           REAL,
    gps_lon           REAL,
    status            TEXT DEFAULT 'raw'  -- raw | edited | published
);
```

- Core opens with `PRAGMA journal_mode=WAL;` and `busy_timeout=2000`.
- Frontend opens read-only using the `immutable=1` URI parameter
  (`file:<path>?immutable=1`) to avoid needing write access for the WAL `-shm` file.
  Frontend never writes to this file.
- `status` lifecycle:
  - `raw` — set on ingest insert.
  - `edited` — set by the XMP watcher when it commits a sidecar change for that file's
    `hash8` prefix (real signal: Lightroom wrote to the file).
  - `published` — set by outgest when a file with a matching `hash8` prefix moves through
    `processed/`. Matching is by embedded filename stem, not re-hashing.

## 2. v1 IPC — trigger files (current Swift button implementation)

| File | Trigger |
|------|---------|
| `~/Photos/.ingest_trigger` | "Run Ingest Now" button |
| `~/Photos/.outgest_trigger` | "Run Outgest Now" button |

Frontend creates an empty file; core polls every 2 seconds, **deletes the file before
acting** (remove-before-act: if the runner fails mid-run, the trigger does not re-fire on
the next tick), then runs the pipeline. No payload — pure signal.

**Planned migration (FL-404 follow-up):** once the Swift app's buttons are wired to the
socket (`ingest_now` / `outgest_now` commands), both trigger files can be retired. Note
that deprecation here when it happens.

## 3. v2 IPC — Unix domain socket (primary, implemented)

**Path:** `~/Library/Application Support/Framelog/framelog.sock`

**Transport:** line-delimited JSON. One connection per request — dial, write one JSON line
+ `\n`, read one JSON line response, close. Not a persistent connection. A core restart
never leaves the frontend holding a dead connection.

**Connection semantics:**
- Frontend dial timeout: 2 seconds.
- No response / connection refused / socket file absent → core unreachable (see §4).
- Server-side `ReadDeadline`: 5 seconds. Silent clients are dropped after this window.
- Socket permissions: 0600 (user-only). Stale socket files from unclean shutdowns are
  removed automatically on server startup.
- Every response includes `"protocol_version": 1` for forward-compatibility detection.

**Requests:**

```json
{"command": "ingest_now"}
{"command": "outgest_now"}
{"command": "status"}
{"command": "set_backup_path", "path": "/Volumes/MyBackupDrive"}
```

`set_backup_path` with an empty `"path"` disables backup. The core persists the
value to `~/Library/Application Support/Framelog/framelog_config.json` and
applies it to the running pipeline immediately — no daemon restart needed. On
startup, the persisted value takes precedence over `FRAMELOG_BACKUP_PATH`.

**Responses:**

```json
{"protocol_version": 1, "ok": true, "imported": 3, "skipped": 1, "failed": 0}
{"protocol_version": 1, "ok": true, "moved": 2, "skipped": 0, "failed": 0}
{"protocol_version": 1, "ok": true, "ingest_running": false, "outgest_running": false,
 "photo_count": 4213, "last_import": "2026-06-20T14:02:00Z", "backup_drive_mounted": true}
{"protocol_version": 1, "ok": true}
{"protocol_version": 1, "ok": false, "error": "ingest_already_running"}
{"protocol_version": 1, "ok": false, "error": "outgest_already_running"}
{"protocol_version": 1, "ok": false, "error": "unknown_command"}
{"protocol_version": 1, "ok": false, "error": "bad_request"}
{"protocol_version": 1, "ok": false, "error": "internal_error"}
```

**Concurrency:** the core holds its own mutex around `RunIngest`/`RunOutgest` and returns
`ingest_already_running` / `outgest_already_running` rather than queuing. The frontend
displays the error; it does not retry automatically.

**`status` is served by a separate handler** that never shares a lock with
`ingest_now`/`outgest_now`. A slow ingest must not make the core look unreachable to a
polling status client.

**Error strings match wire JSON exactly.** `ErrIngestAlreadyRunning.Error()` returns
`"ingest_already_running"` — used verbatim as the JSON `"error"` field value.

## 4. Core reachability and status display (FL-604)

The Swift app determines display state by combining a **socket ping** (POSIX `connect()`
to the socket path — returns in microseconds) with the **DB snapshot**:

| Socket | DB | Display |
|--------|-----|---------|
| down | missing | "Install Core to get started" |
| down | exists | "Core restarting…" (launchd recovering from crash) |
| up | empty | "No photos imported yet" |
| up | has photos | "N photos · last import: X ago" |

"Core restarting…" is the key distinction: when `framelogd` crashes, launchd restarts it
automatically (`KeepAlive=true`). During that window `catalog.db` still exists from the
last run. Without the socket ping, the menu bar would silently show stale data as if
everything were fine.

## 5. Notification / freshness model

No push channel. The frontend polls `status` on a 15-second timer and diffs
`last_import` / `photo_count` against the previous poll. If `last_import` advanced, a
local `UserNotification` fires. This handles SD-card-triggered ingests (which happen
entirely inside the core with no frontend request).

**Do not change the 15-second poll interval** without also updating this document and
verifying the UNUserNotificationCenter throttling does not suppress rapid successive
notifications.

## 6. XMP / git behaviour

**Non-DNG files** (RAF, CR3, ARW, HEIC, JPG): Lightroom writes a `.xmp` sidecar next to
the file. The XMP watcher detects the write via fsnotify, waits 10 seconds (debounce to
collapse burst edits), then runs `git add -A && git commit` in `originals/`.

**DNG files**: DNG embeds XMP internally. Lightroom writes edits back into the DNG file
itself (not a sidecar). Ingest does **not** create a `.xmp` sidecar for DNG files —
doing so causes Lightroom to read the sidecar instead of the embedded data, hiding develop
edits. The XMP watcher detects a WRITE event on the `.dng` file, then runs
`exiftool -xmp -b <file>` to extract the embedded XMP packet and writes it to a `.xmp`
sidecar. The sidecar is then committed by git. This keeps the git history small (XMP
bytes only, not the full DNG binary).

**Formats that embed XMP** are defined in `config.EmbeddedXMPExtensions`. Adding a new
format here automatically applies the extraction behaviour in both ingest and xmpwatcher.

**`originals/` git tracking:**

```gitignore
# managed by framelogd — only XMP sidecars are tracked
*
!*/
!*.xmp
!.gitignore
```

`!*/` is required to un-ignore subdirectories. Without it, `*` ignores `2025/` and
`!*.xmp` never applies to files inside it.

**Push gate:** after committing, the watcher pushes only if both:
- `pmset` reports AC power, **and**
- `pgrep -i lightroom` finds no match (Lightroom is closed)

If either binary is absent, that gate is skipped.

## 7. Log line format

```
2026-06-22 14:03:11 [INGEST] Done: 3 imported, 1 skipped, 0 failed
```

`TIMESTAMP [PREFIX] message` — one logger, always flushed (`fsync` on every write).

Closed set of prefixes — add to this list in the same commit that introduces a new one:

`CORE` · `INGEST` · `OUTGEST` · `XMP` · `GIT` · `BACKUP`

Frontend tails this file for the log viewer. No bare `print()` anywhere in the core.

## 8. File layout on disk

```
~/Photos/
├── inbox/                    ← SD card landing zone, cleared after import
├── originals/YYYY/MM/DD/     ← imported files + .xmp sidecars; git-tracked
│   └── .git/                 ← tracks .xmp sidecars only (see gitignore above)
├── processed/YYYY/MM/        ← Lightroom exports, organised by outgest
├── catalog.db                ← SQLite WAL; core writes, frontend reads immutable
└── framelog.log              ← structured log

~/Library/LaunchAgents/
└── com.framelog.core.plist   ← installed by `framelogd install` (FL-303)

~/Library/Application Support/Framelog/
└── framelog.sock             ← v2 IPC socket (FL-302)

~/Library/Logs/Framelog/
└── crash.log                 ← launchd stdout/stderr; empty in normal operation
```

## 9. Resolved design decisions

- **`crs:` XMP namespace** — out of scope. Nothing in this pipeline writes develop-settings
  XMP; Lightroom owns those tags via its own writes. Not registered in `core/xmp`.
- **`gitops` uses the `git` CLI**, not `go-git`. Matches tested Python behaviour; the
  injectable-path pattern makes it testable without a real git on PATH in CI.
- **Backup uses `rclone copy`**, not `rclone sync`. A bad delete on the source must never
  propagate to the backup.
- **`outgestwatcher` watches only the top-level `processed/` directory**, never
  subdirectories. `YYYY/MM/` folders created by a prior outgest run must not trigger a
  re-scan of already-organised files.
- **XMP watcher skips `.git/`** during initial walk. Watching git internals would leak
  fsnotify watches on every commit the watcher itself makes.
- **Socket path is capped at 104 bytes** (macOS `sockaddr_un` limit). Tests use
  `os.MkdirTemp("", "prefix*")` under `/tmp` (short paths), not `t.TempDir()` (long
  paths under the test cache). Violations surface as `bind: invalid argument`.
