# CLAUDE.md — Context for AI assistants working on Framelog

## What this project is

A macOS photo pipeline rewritten from Python into a Go daemon (`framelogd`)
and a Swift menu bar app (`Framelog.app`). The Python predecessor is fully
decommissioned. There are no `src/framelog/` Python files, no `menubar.py`,
no `on_sd_mount.sh`. When you encounter references to those, they are
historical — do not restore them.

## Repo layout

```
core/         Go module (github.com/thevedantmodi/framelog/core)
  cmd/framelogd/    main package: run/install/uninstall/--version
  activity/         live progress tracker (phase/done/total) read by status
  backup/           rclone wrapper; IsDriveMounted; DestDir
  cardverify/       end-to-end "safe to format" check, instant card preview,
                    quick-hash backfill, plain-text receipts
  config/           all paths and constants (single source of truth);
                    filetypes.go = runtime-settable import types
  db/               SQLite schema + queries
  exif/             exiftool subprocess wrapper
  gitops/           git CLI wrapper; pmset AC-power check
  hasher/           SHA-256
  ingest/           RunIngest; concurrency guard (TryAcquire/Release)
  ipc/              Unix socket server; status/ingest_now/outgest_now
  launchd/          plist generation; Install/Uninstall
  logging/          structured logger (PREFIX, fsync on every write)
  nrtmeta/          Sony NonRealTimeMeta XML sidecar parser (XAVC clips)
  outgest/          RunOutgest; concurrency guard
  outgestwatcher/   fsnotify watcher for processed/; debounce
  rclonerun/        shared rclone JSON-log runner; per-file errors; Explain
  sim/              end-to-end simulator: fake card + fake tools, real pipeline
  cmd/framelog-sim/ CLI for sim (make sim)
  sdcard/           /Volumes watcher; diskutil check; DCIM + XAVC CLIP copy;
                    copy → ingest → verify card run; VerifyMounted
  triggerwatcher/   2s poll for .ingest_trigger / .outgest_trigger
  xmp/              sidecar writer
  xmpwatcher/       fsnotify watcher for originals/; debounce+commit
menubar/      Xcode project; Swift menu bar app
docs/         PROTOCOL.md (frozen contract), ROADMAP.md, PHASE5_RUNBOOK.md
```

## The three contracts you must not break

1. **`docs/PROTOCOL.md`** — the only thing Go and Swift need to agree on.
   Any change to IPC shapes, trigger-file semantics, log format, or DB schema
   must be reflected here in the same commit. Do not let the doc drift.

2. **Injectable binary paths** — no package calls `exec.LookPath` inside a
   function that does real work. `FindExiftool`/`FindGit`/`FindDiskutil`/etc.
   return paths; action functions (`ReadExif`, `Commit`, `IsRemovableMedia`)
   accept paths as parameters. Tests inject fake shell scripts. Never break
   this pattern — CI runs on ubuntu-latest where diskutil/pmset/pgrep do not
   exist.

3. **`config` is the single source of truth for paths.** Nothing is
   hardcoded a second time in any other package. The one exception is launchd's
   `CrashLogPath` (derived from homeDir at template-render time, not from
   `config.CrashLogPath`), which exists for a specific reason: the logger
   already writes to `config.LogFile` AND stdout, so pointing launchd's
   `StandardOutPath` at `LogFile` would duplicate every structured log line.
   `CrashLogPath` captures only panics and pre-logger failures. A test
   asserts this invariant (`TestGeneratePlist_UsesCrashLogPathNotLogFile`).

## Build and test

```bash
cd core

# Build (no version stamp):
go build -o framelogd ./cmd/framelogd

# Build (with version stamp for release):
go build -ldflags "-X main.Version=$(cat ../VERSION)" -o framelogd ./cmd/framelogd

# Test everything under the race detector:
go test ./... -race

# Test a single package verbosely:
go test ./ipc/... -race -v

# See the whole pipeline work end to end, no SD card needed (narrated):
make sim                      # or: go run ./cmd/framelog-sim -v
```

`core/sim` drives the real watcher → preview → copy → ingest → backup →
verify path and the IPC socket against a generated Sony card in a sandbox,
with fake diskutil/exiftool/rclone/pmset (`-real-rclone` for the installed
one). Its scenarios also run as `go test ./sim` in CI. When you change card,
backup or verification behaviour, add or update a scenario in
`core/sim/scenarios.go` — it is the closest thing to inserting a real card.

## Key design decisions (don't re-litigate without reading the rationale)

**`status` handler must never share a lock with `ingest_now`/`outgest_now`.**
The `StatusProvider` interface reads only the mutex-guarded `running` bool and
two fast DB queries. A slow ingest must not make the core look unreachable to
a polling status client.

**Remove-before-act for trigger files.** The trigger file is deleted *before*
calling the runner. If the runner fails mid-run, the file is already gone, so
the same trigger does not fire again on the next tick.

**`run(rc *runConfig, stop <-chan struct{}) error`** is the testable core of
the daemon. `mainRun()` does binary resolution + workspace init + DB open +
wiring, then calls `run()`. Tests build their own `runConfig` with fake
binaries and call `run()` directly. Never collapse these two functions.

**`initWorkspace`** creates `inbox/`, `originals/`, `processed/`, and runs
`git init originals/` on first start. Idempotent — safe on every startup.
It is called from `mainRun()` after the logger is open so errors are logged.

**Trigger files vs. socket:** The Swift app's "Run Ingest Now" / "Run Outgest
Now" buttons currently use `.ingest_trigger` / `.outgest_trigger` (v1 IPC).
The socket exists and is tested. The migration from trigger files to socket
commands is pending (FL-404 follow-up). Until that lands, both mechanisms
must keep working.

**KeepAlive + CrashLogPath:** The launchd plist sets `KeepAlive=true`. If
the daemon panics, launchd restarts it. The panic output goes to
`~/Library/Logs/Framelog/crash.log`, not to `~/Photos/framelog.log`. In
normal operation `crash.log` is empty — anything in it after a restart is a
real signal.

## Unix socket path length limit

macOS caps Unix domain socket paths at 104 bytes. Tests that create socket
files must use `os.MkdirTemp("", "prefix*")` (short paths under `/tmp`) rather
than `t.TempDir()` (which produces long paths under the test cache). See
`core/ipc/ipc_test.go:shortTempDir`. Violations surface as
`bind: invalid argument` at runtime.

## Things that look wrong but are intentional

- `backup.Sync` calls `rclone copy`, not `rclone sync`. Deliberate: a bad
  delete on the source must never propagate to the backup.
- `outgestwatcher` watches only the top-level `processed/` directory, never
  subdirectories. Deliberate: `YYYY/MM/` folders created by a prior outgest run
  must not trigger a re-scan of already-organised files.
- File types live in `config/filetypes.go` behind accessors, not a map: the
  import set is user-settable at runtime (`set_import_types`), so read it with
  `config.IsImportable` / `config.ImportExtensions()`, never cache it.
  `outgest`/`outgestwatcher` use `config.IsOutgestExtension`, which is every
  known type + every imported type + export-only types (just PNG). PNG
  exports are filed but never imported; working files (`.psb`/`.psd`/`.jxl`)
  stay out because outgest sets `status="published"` and they share the
  export's hash8 prefix. Tests in `core/config/config_test.go` pin this.
- Every `KnownMediaTypes` entry defaults to imported. Deliberate: a known
  type the user switched off is "ignored" (doesn't block safe-to-format), so a
  type that started off would be dropped without the user ever choosing.
- The card preview uses a quick hash (size + first/last 64 KiB) and decides
  what gets copied, but never the verdict — full SHA-256 verification does.
  A wrong quick match is caught by verification and the skipped files are
  copied in the same run. See PROTOCOL.md §11.
- Settings are persisted with `config.UpdateUserConfig` (read-modify-write),
  never `WriteUserConfig` with a fresh struct, so one setting can't reset
  another.
- Sony XAVC clips are read from `PRIVATE/M4ROOT/CLIP/`, not `DCIM/`, and their
  `C####M01.XML` sidecar overrides exiftool for capture date and camera model.
  Deliberate: the MP4 container stores only a UTC `CreateDate` with no offset,
  which files an evening shoot under the next day, and names no camera body.
  The sidecar is copied next to the imported clip (`.gitignore` tracks only
  `*.xmp`, so it stays untracked) and deleted from `inbox/` with the clip —
  nothing else in the pipeline collects `.xml`, so one left behind is stuck.
- The XMP watcher skips `.git/` during its initial walk. Deliberate: watching
  git internals would leak fsnotify watches on every commit the watcher itself
  makes.
- `ErrIngestAlreadyRunning` / `ErrOutgestAlreadyRunning` error strings match
  the wire JSON `"error"` field exactly (`"ingest_already_running"` /
  `"outgest_already_running"`). The IPC handler uses `err.Error()` directly as
  the JSON value — no mapping table.
- Card verification re-reads every file on the card and re-hashes the
  library and backup copies instead of trusting rclone's exit code, the
  inbox copy, or the catalog. Deliberate: the whole point is a verdict the
  user can format on. It looks files up by content hash, so it is
  indifferent to how a file reached the library. See PROTOCOL.md §10.
- A configured-but-unplugged backup drive makes the card verdict "don't
  format", while no backup configured at all is only a note. Deliberate:
  a configured backup means the user asked for two copies, and only one is
  provable. `unhandled` file types on the card also block — formatting
  would silently destroy them.
- `.hif` is a known (default-on) type: Sony bodies (incl. the α7C II) name
  HEIF stills `.HIF`, not `.HEIC`. Without it a HEIF card would never be
  imported and the verifier would flag every still as unhandled.
- Ingest retries a pending backup (failed / drive missing / destination
  changed) even when nothing new was imported, and records every attempt
  in `backup_runs`. `RunBackup` shares ingest's running flag so a backup
  never overlaps an import.
- The launchd plist is re-generated on every `framelogd install`, not cached.
  This ensures the plist always references the current executable path even if
  you moved or rebuilt the binary.

## What's not done yet (Phase 6)

- **FL-404 socket migration:** Swift app buttons still use trigger files.
  Once migrated, both `.ingest_trigger` and `.outgest_trigger` can be retired
  from the protocol.
- **FL-601 codesigning/notarization:** deferred — requires a $99 Apple
  Developer account. App runs unsigned; Homebrew users install with
  `--no-quarantine`.

Phase 5 (migration/cutover) and the rest of Phase 6 (FL-602/603/604/605 —
version stamping, installer DMG, crash/restart policy, release CI) are done.
See `docs/ROADMAP.md` for the authoritative status.
