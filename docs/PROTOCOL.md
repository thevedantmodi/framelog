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

-- One row per end-to-end SD card verification (see §10).
CREATE TABLE IF NOT EXISTS card_reports (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    volume_name      TEXT,     -- e.g. "Untitled" (Sony's default)
    volume_uuid      TEXT,     -- diskutil "Volume UUID"; changes on every format
    started_at       TEXT,     -- ISO 8601, UTC
    finished_at      TEXT,
    files_total      INTEGER,  -- importable media files on the card
    bytes_total      INTEGER,
    files_verified   INTEGER,
    files_problem    INTEGER,  -- every listed file that is not "verified"
    files_new        INTEGER,  -- verified, imported during this card session
    files_existing   INTEGER,  -- verified, already in the library beforehand
    file_types       TEXT,     -- "ARW 312 · JPG 312 · MP4 4"
    camera_models    TEXT,     -- comma-separated
    capture_first    TEXT,     -- exiftool layout "YYYY:MM:DD HH:MM:SS"
    capture_last     TEXT,
    backup_state     TEXT,     -- verified | not_configured | drive_missing | incomplete
    safe_to_format   INTEGER,  -- 0/1, the verdict
    problems         TEXT,     -- newline-separated human-readable sentences
    report_path      TEXT      -- plain-text receipt in ~/Photos/reports/
);

CREATE TABLE IF NOT EXISTS card_report_files (
    report_id     INTEGER REFERENCES card_reports(id),
    card_path     TEXT,   -- relative to the volume root
    size          INTEGER,
    hash          TEXT,   -- SHA-256 of the bytes on the card
    library_path  TEXT,
    backup_path   TEXT,
    state         TEXT,   -- see §10 file states
    detail        TEXT
);

-- One row per backup attempt (after ingest, or backup_now).
CREATE TABLE IF NOT EXISTS backup_runs (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at    TEXT,
    finished_at   TEXT,
    destination   TEXT,     -- backup root; copies live in <destination>/originals/
    outcome       TEXT,     -- ok | failed | drive_missing | rclone_missing
    files_copied  INTEGER,
    files_failed  INTEGER,  -- distinct files rclone reported errors for
    error         TEXT      -- human-readable explanation, "" on success
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
  This deadline covers only reading the request. `ingest_now` / `outgest_now` run the
  pipeline synchronously and reply when it finishes — potentially minutes later. Clients
  must not apply the 2-second dial timeout to the response read; read until newline or
  connection close.
- Socket permissions: 0600 (user-only). Stale socket files from unclean shutdowns are
  removed automatically on server startup.
- Every response includes `"protocol_version": 1` for forward-compatibility detection.

**Requests:**

```json
{"command": "ingest_now"}
{"command": "outgest_now"}
{"command": "status"}
{"command": "set_backup_path", "path": "/Volumes/MyBackupDrive"}
{"command": "pause"}
{"command": "resume"}
{"command": "verify_card"}
{"command": "backup_now"}
```

`verify_card` re-runs the §10 end-to-end check on the currently mounted
camera card — no copy, no import — and replies when it finishes (as long as
re-reading the card takes). Read-only, so it is allowed while paused. Use it
after reconnecting the backup drive, or right before formatting.

`backup_now` copies `originals/` to the configured backup drive now (the
"Retry Backup" action). It shares ingest's running flag, so it returns
`ingest_already_running` during an import and `ingest_paused` while paused.
A completed attempt is `ok: true` whatever its outcome — the outcome is in
`outcome` / `backup_error`, and is also recorded in `backup_runs`.

**Automatic backup retry:** after every ingest, the backup runs if anything
was imported **or** a backup is pending (the last attempt failed, the drive
was missing, or the destination changed since the last success) — a failed
backup is retried by the next ingest instead of waiting for new photos.

`set_backup_path` with an empty `"path"` disables backup. The core persists the
value to `~/Library/Application Support/Framelog/framelog_config.json` and
applies it to the running pipeline immediately — no daemon restart needed. On
startup, the persisted value takes precedence over `FRAMELOG_BACKUP_PATH`.

`pause` / `resume` are a single global toggle covering ingest **and** outgest
together — there is no separate "pause ingest only" mode. While paused:
- `ingest_now` / `outgest_now` return `ingest_paused` / `outgest_paused` instead
  of running.
- The trigger-file watcher (§2) leaves `.ingest_trigger` / `.outgest_trigger` in
  place instead of consuming them, so the pending request fires once resumed.
- The SD card watcher does not copy card contents (`DCIM/`,
  `PRIVATE/M4ROOT/CLIP/`) or mark the card processed;
  it re-checks the card on every poll tick until resumed, so no physical
  unmount/remount is needed to retry.
- The XMP-triggered outgest watcher (`outgestwatcher`) still logs the fsnotify
  event but skips the run; a later export/manual `outgest_now` after resume
  picks up any files left in `processed/`.

Pause state is in-memory only — it resets to unpaused on every daemon restart
(including a `launchd` `KeepAlive` crash-restart).

**Responses:**

```json
{"protocol_version": 1, "ok": true, "imported": 3, "skipped": 1, "failed": 0}
{"protocol_version": 1, "ok": true, "moved": 2, "skipped": 0, "failed": 0}
{"protocol_version": 1, "ok": true, "ingest_running": false, "outgest_running": false,
 "photo_count": 4213, "last_import": "2026-06-20T14:02:00Z", "backup_drive_mounted": true,
 "backup_configured": true, "daemon_version": "0.4.0", "paused": false,
 "capabilities": {"sd_card_watch": true, "backup": true, "ac_power_gate": true,
                  "lightroom_check": true},
 "activity": {"phase": "verifying", "volume": "Untitled", "done": 120, "total": 412,
              "current_file": "DSC00120.ARW", "started_at": "2026-11-07T18:03:11Z"},
 "backup": {"pending": false, "last_outcome": "ok", "last_error": "",
            "last_run_at": "2026-11-07T18:01:40Z", "last_success_at": "2026-11-07T18:01:40Z",
            "files_copied": 412, "files_failed": 0},
 "last_card": {"id": 12, "volume_name": "Untitled", "volume_uuid": "0E1D2C3B-…",
               "started_at": "2026-11-07T17:58:02Z", "finished_at": "2026-11-07T18:09:30Z",
               "safe_to_format": true, "files_total": 412, "bytes_total": 23400000000,
               "files_verified": 412, "files_problem": 0, "files_new": 400, "files_existing": 12,
               "file_types": "ARW 206 · JPG 206", "camera_models": "ILCE-7CM2",
               "capture_first": "2026:11:07 09:12:44", "capture_last": "2026:11:07 16:40:02",
               "backup_state": "verified", "problems": [],
               "report_path": "/Users/me/Photos/reports/20261107_175802_Untitled.txt"}}
{"protocol_version": 1, "ok": true, "card": { …same shape as last_card… }}
{"protocol_version": 1, "ok": true, "outcome": "failed", "files_copied": 10,
 "files_failed": 2, "backup_error": "Backup drive is full"}
{"protocol_version": 1, "ok": true}
{"protocol_version": 1, "ok": true, "paused": true}
{"protocol_version": 1, "ok": false, "error": "ingest_already_running"}
{"protocol_version": 1, "ok": false, "error": "outgest_already_running"}
{"protocol_version": 1, "ok": false, "error": "ingest_paused"}
{"protocol_version": 1, "ok": false, "error": "outgest_paused"}
{"protocol_version": 1, "ok": false, "error": "no_card"}
{"protocol_version": 1, "ok": false, "error": "card_busy"}
{"protocol_version": 1, "ok": false, "error": "verify_unavailable"}
{"protocol_version": 1, "ok": false, "error": "backup_not_configured"}
{"protocol_version": 1, "ok": false, "error": "unknown_command"}
{"protocol_version": 1, "ok": false, "error": "bad_request"}
{"protocol_version": 1, "ok": false, "error": "internal_error"}
```

**Concurrency:** the core holds its own mutex around `RunIngest`/`RunOutgest` and returns
`ingest_already_running` / `outgest_already_running` rather than queuing. The frontend
displays the error; it does not retry automatically. The `paused` check happens before
the mutex is even attempted, so `ingest_paused`/`outgest_paused` and
`ingest_already_running`/`outgest_already_running` are mutually exclusive outcomes.

**Degraded-capability surfacing:** `capabilities` reports which optional binaries the
daemon found at startup (`sd_card_watch`=diskutil, `backup`=rclone, `ac_power_gate`=pmset,
`lightroom_check`=pgrep). A `false` value means that feature is disabled for this daemon
run; the frontend shows it in the menu instead of requiring a log tail. Static per daemon
process. `backup_configured` distinguishes "no backup path set" from "backup drive
unplugged" — both report `backup_drive_mounted: false` but call for different guidance.

**Report fields in `status`** (all additive; a client must tolerate their absence,
which is what an older daemon sends):
- `activity` — live progress. `phase` is one of `idle` · `copying` (card → inbox) ·
  `importing` (inbox → originals) · `backing_up` · `verifying`. `total: 0` means
  unknown (backup doesn't know its total up front). `volume` is set for card phases.
- `backup` — the most recent `backup_runs` row plus `pending` (see "Automatic backup
  retry" above; always `false` when no backup is configured, and `false` on an empty
  library). `last_error` is a plain sentence ("Backup drive is full"), not rclone output.
- `last_card` — the newest `card_reports` row. Absent until the first card has been
  verified. `problems` is always an array.

`verify_card` errors: `no_card` (no removable volume with `DCIM/` or
`PRIVATE/M4ROOT/CLIP/`), `card_busy` (a card is being copied/verified right now),
`verify_unavailable` (SD card watcher disabled — diskutil or rclone missing).

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

The same poll drives two more notifications, each fired only on a transition (the
values present on the app's first poll seed the comparison and never notify):
- `last_card.id` changed → "Untitled is safe to format" / "Don't format Untitled yet"
  with the first problem.
- `backup.last_run_at` changed and `backup.last_outcome == "failed"` → "Backup failed"
  with `last_error`.

**Do not change the 15-second idle poll interval** without also updating this document and
verifying the UNUserNotificationCenter throttling does not suppress rapid successive
notifications. While `activity.phase != "idle"` the frontend polls every **2 seconds**
so progress in the open menu moves; it drops back to 15 s as soon as the core reports
idle (or stops answering). Notifications are transition-only, so the fast cadence
cannot multiply them.

## 6. XMP / git behaviour

**Non-DNG files** (RAF, CR3, ARW, HEIC, HIF, JPG): Lightroom writes a `.xmp` sidecar next to
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

Card verification logs under `CORE` (the SD card watcher's prefix):
`card Untitled: SAFE TO FORMAT — 412/412 files verified (23.4 GB), backup verified`,
followed by one `  problem: …` line per problem and `card report: <path>`.
Backup failures log `[BACKUP] FAILED: <explanation> (N copied, M failed)` followed by up
to 10 `  not backed up: <path>` lines.

Frontend tails this file for the log viewer. No bare `print()` anywhere in the core.

## 8. File layout on disk

```
~/Photos/
├── inbox/                    ← SD card landing zone, cleared after import
│   ├── duplicates/           ← sources whose hash is already in the catalog;
│   │                           parked by ingest, never re-scanned, user-reviewable
│   └── failed/               ← sources that failed import 3 times (zero-byte,
│                               exiftool errors); quarantined, never re-scanned
├── originals/YYYY/MM/DD/     ← imported files + .xmp sidecars; git-tracked
│   └── .git/                 ← tracks .xmp sidecars only (see gitignore above)
├── processed/YYYY/MM/        ← Lightroom exports, organised by outgest
├── reports/                  ← one plain-text receipt per verified card (§10)
│   └── YYYYMMDD_HHMMSS_<volume>.txt
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
- **Outgest filters on `config.OutgestExtensions`, not `config.SupportedExtensions`.**
  The import list and the export list are deliberately different: ingest decides what
  enters `originals/` and gets git-tracked, outgest decides what counts as a finished
  export. A PNG export must be filed into `processed/YYYY/MM/` but must never be
  ingested. The export-only list stays narrow because outgest sets
  `status="published"`: Photoshop working files and archival intermediates share the
  export's hash8 prefix and would republish the same catalog row.
  `OutgestExtensions` is built from `SupportedExtensions` at init, so the import formats
  can never drift out of the export set; export-only additions go in
  `outgestOnlyExtensions`.
- **XMP watcher skips `.git/`** during initial walk. Watching git internals would leak
  fsnotify watches on every commit the watcher itself makes.
- **Socket path is capped at 104 bytes** (macOS `sockaddr_un` limit). Tests use
  `os.MkdirTemp("", "prefix*")` under `/tmp` (short paths), not `t.TempDir()` (long
  paths under the test cache). Violations surface as `bind: invalid argument`.

## 10. End-to-end card verification ("safe to format")

After every automatic card run (copy → ingest → backup) the SD card watcher re-reads
the card and proves every file made it, then records a verdict. `verify_card` runs the
same check on demand. Implemented in `core/cardverify`.

**What is checked.** Every non-hidden file under `DCIM/` and `PRIVATE/M4ROOT/CLIP/` is
inventoried (hidden `._*` / `.DS_Store` / `.Trashes` are Mac-created and skipped; other
card directories — Sony thumbnails, proxies, databases — are regenerated by the camera
and not inventoried). Each file is classified:
- **media** — extension in `config.SupportedExtensions`; must be verified.
- **sidecar** — `.xml` inside the clip directory (Sony NonRealTimeMeta); copied with its
  clip, counted, not separately verified.
- **unhandled** — anything else in a media directory. Formatting would destroy it, so
  it blocks the verdict.

For each media file: SHA-256 the bytes **on the card**; look the hash up in `photos`;
re-hash the library file it points at; when a backup is configured and mounted, re-hash
`<backup>/originals/<same relative path>`. Size is compared first, so truncation is
caught without a full read. Because `photos` is keyed by content hash, a file counts no
matter which import put it there (this session, an earlier insert, another card).

**File states** (`card_report_files.state`): `verified` · `not_in_library` ·
`library_missing` · `library_mismatch` · `backup_missing` · `backup_mismatch` ·
`card_unreadable` · `unhandled`.

**Backup states** (`card_reports.backup_state`): `verified` · `not_configured` (library
is the only copy — reported as a note, does not block) · `drive_missing` (blocks: the
user configured two copies, only one is provable) · `incomplete` (blocks).

**Verdict.** `safe_to_format` is true iff every media file is `verified`, there are no
`unhandled` files, and `backup_state` is `verified` or `not_configured`. A card with no
media is safe. Copy errors from the card run are listed in `problems` as context but do
not decide the verdict — the per-file check does.

**Receipt.** `~/Photos/reports/YYYYMMDD_HHMMSS_<volume>.txt` (local time; never
overwritten — a same-second re-check gets `-2`): verdict, card identity, counts, types,
camera, capture range, new vs. already-imported, library/backup tallies, problems, then
every file with its state, problem files first.

**Identity.** Card volume names are not unique (every Sony card is "Untitled"), so
reports carry diskutil's Volume UUID, which also changes when the card is reformatted.

