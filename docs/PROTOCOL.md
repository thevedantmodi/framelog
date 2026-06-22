# PROTOCOL.md — Framelog core ↔ frontend contract

This is the only thing the Go core and the Swift menu bar need to agree on. Either side
should be buildable from this document alone, without reading the other side's source.

Status: **resolved draft** — the three open questions from the first pass are decided
below (§6, with rationale, since this is a solo project rather than a team that needs
sign-off). Flip any of them before starting Phase 1 if you disagree; just update this
file in the same commit. Treat changes to this file as part of the same PR that changes
the contract (Definition of Done, roadmap).

---

## 1. `catalog.db` schema (frozen contract — core writes, frontend reads)

```sql
CREATE TABLE IF NOT EXISTS photos (
    hash              TEXT PRIMARY KEY,
    original_filename TEXT,
    imported_path     TEXT,
    camera_model      TEXT,
    capture_date      TEXT,
    import_timestamp  TEXT,
    gps_lat           REAL,
    gps_lon           REAL,
    status            TEXT DEFAULT 'raw'  -- raw | edited | published
);
```

- Core opens with `PRAGMA journal_mode=WAL;` and a `busy_timeout` of at least 2000ms.
  (FL-004)
- Frontend opens **read-only** (`?mode=ro` DSN or equivalent). Never writes to this file.
- **Decision: `status` is kept, but only auto-set by two pipeline events, not three.**
  `culled` is dropped from the enum — nothing in the current pipeline has a signal for
  "the user decided to keep/discard this," so it would ship unused exactly like the
  Python version's lifecycle did. The two that *do* have a real trigger:
  - `ingest` sets `raw` on insert (FL-201, unchanged).
  - The XMP watcher (FL-204) sets `edited` on the matching `hash` when it commits a
    sidecar change for that file — this is a real signal (Lightroom touched the XMP) and
    costs nothing extra to wire, since the watcher already knows which file changed.
  - `outgest` (FL-202) sets `published` when a file with a matching hash moves through
    `processed/`. Matching is by filename stem (`<hash8>` is embedded in the original
    filename) since outgest doesn't re-hash exports.
  - If you don't want this at all, simplest alternative is dropping the column entirely —
    just don't leave it half-wired a second time.

## 2. v1 IPC — trigger file (FL-301)

- Path: `~/Photos/.ingest_trigger`
- Frontend creates an empty file at this path to request an ingest run.
- Core polls for its existence (or watches it via the same `fsnotify` instance used for
  FL-203/204, to avoid a second poll loop), deletes it, then runs ingest. No payload —
  pure signal, not a command channel.
- **Why this still exists even though the SD-card watcher moved in-process (FL-203):**
  in the Python version, this file was the *only* way `on_sd_mount.sh` (a separate bash
  process) could tell `menubar.py` to ingest. In the Go core, SD-card-triggered ingest
  needs no IPC at all anymore — the watcher just calls `ingest.RunIngest()` directly in
  the same binary. The trigger file's only remaining job is the frontend's manual
  "Run Ingest Now" button, as the fast path to get that working before FL-302 exists.
  Once FL-404 is wired to the socket, this file can be deleted from the contract — note
  that deprecation here when it happens, don't just let it linger unused.

## 3. v2 IPC — Unix domain socket (FL-302)

- Path: `~/Library/Application Support/Framelog/framelog.sock`
- Transport: line-delimited JSON. **One connection per request** — dial, write one JSON
  line + `\n`, read one JSON line response, close. Not a persistent/streaming connection.
  This keeps the frontend's reconnect logic trivial (just dial again next time) and means
  a core restart never leaves the frontend holding a dead connection.
- Frontend dial timeout: 2 seconds. No response (or connection refused, or the socket
  file doesn't exist) within that window = render "core unreachable" (FL-604). Don't
  retry more than once before showing that state — a hung core shouldn't make the menu
  bar itself feel unresponsive.
- Every response includes `"protocol_version": 1` so a future frontend/core pairing that
  drifts out of sync fails loudly instead of silently misparsing fields.

**Requests:**

```json
{"command": "ingest_now"}
{"command": "outgest_now"}
{"command": "status"}
```

**Responses:**

```json
{"protocol_version": 1, "ok": true, "imported": 3, "skipped": 1, "failed": 0}
{"protocol_version": 1, "ok": true, "moved": 2, "skipped": 0, "failed": 0}
{"protocol_version": 1, "ok": true, "ingest_running": false, "outgest_running": false, "photo_count": 4213, "last_import": "2026-06-20T14:02:00Z", "backup_drive_mounted": true}
{"protocol_version": 1, "ok": false, "error": "ingest_already_running"}
{"protocol_version": 1, "ok": false, "error": "unknown_command"}
```

- **Concurrency is the core's job, not the frontend's.** In the Python version, the
  `_ingest_running` guard lived in `menubar.py` (the frontend) — that only worked because
  ingest and the UI shared a process. Now that the SD-card watcher and a manual
  `ingest_now` request can race from two different goroutines inside the *core*, the core
  must hold its own mutex/flag around `RunIngest`/`RunOutgest` and return
  `{"ok": false, "error": "ingest_already_running"}` rather than queuing or blocking the
  caller. The frontend just displays that error; it doesn't retry automatically.
- `status` must be served by a separate, always-available handler — don't let it share a
  lock with `ingest_now`/`outgest_now`, or a slow ingest makes the core *look*
  unreachable when it's actually just busy. That distinction (busy vs. unreachable)
  matters to FL-604.
- Unknown/malformed JSON on the request line → `{"ok": false, "error": "bad_request"}`,
  connection closed after responding. Don't crash the listener goroutine on bad input.

## 4. Notification / freshness model (resolves the gap in the original draft)

The original draft only covered request/response, but FL-405 (notifications) needs the
frontend to learn about ingests it didn't ask for — an SD card mounting fires ingest
entirely inside the core, with no frontend request involved at all. One-shot
connect/respond has no channel for the core to push that.

**Decision: no push channel. The frontend polls `status` on a timer (15s — frequent
enough to feel responsive, cheap enough since it's a local socket round-trip, not a DB
query) and diffs `last_import`/`photo_count` against its previous poll.** If
`last_import` advanced since the last poll, fire a local `UserNotification`. This is the
same pattern `menubar.py`'s `@rumps.timer(30)` → `_refresh_status()` already used; it's
just relocated to the other side of an IPC boundary now. Keeps both ends simple — no
second message type, no long-lived connection to manage reconnection logic for.

## 5. Log line format

```
2026-06-22 14:03:11 [INGEST] Done: 3 imported, 1 skipped, 0 failed
```

`TIMESTAMP [PREFIX] message`, one logger, always flushed. Closed set of prefixes —
add to this list in the same PR that introduces a new one, don't invent ad hoc tags:

`INGEST` · `OUTGEST` · `XMP` · `BACKUP` · `GIT` · `CORE`

Frontend tails this file for the log viewer (FL-403/FL-406). No bare
`print()`-equivalent anywhere in the core — the Python version's `ingest.py` vs.
`outgest.py` split doesn't get a sequel here.

## 6. Resolved decisions (were open questions in the first draft)

- **`crs:` XMP namespace — out of scope for v1.** Nothing in this pipeline ever wrote
  develop-settings XMP (Lightroom owns those tags via its own writes); the Python
  version registered the namespace but never emitted a `crs:` element. Don't register
  it in `xmp` (FL-105) unless a real feature needs it later.
- **`gitops` — shell out to the `git` CLI, not `go-git`, for v1.** Matches the Python
  version's tested behavior exactly (same commands, same output parsing), and it's an
  internal package behind a small interface (`Commit`/`Push`) — swapping the
  implementation to `go-git` later, if the CLI dependency ever becomes a real problem,
  doesn't change anything on the other side of that interface.
- **Backup target — confirmed: `rclone copy` of `originals/` to `BACKUP_PATH`, run after
  a successful ingest batch (FL-207).** Replaces the old (disabled) Python behavior of
  syncing the raw SD card pre-ingest. `copy`, not `sync` — a bad delete on the source
  side must never propagate to the backup.

## 7. File layout on disk

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
