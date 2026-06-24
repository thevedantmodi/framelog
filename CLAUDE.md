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
  backup/           rclone wrapper; IsDriveMounted
  config/           all paths and constants (single source of truth)
  db/               SQLite schema + queries
  exif/             exiftool subprocess wrapper
  gitops/           git CLI wrapper; pmset AC-power check
  hasher/           SHA-256
  ingest/           RunIngest; concurrency guard (TryAcquire/Release)
  ipc/              Unix socket server; status/ingest_now/outgest_now
  launchd/          plist generation; Install/Uninstall
  logging/          structured logger (PREFIX, fsync on every write)
  outgest/          RunOutgest; concurrency guard
  outgestwatcher/   fsnotify watcher for processed/; debounce
  sdcard/           /Volumes watcher; diskutil check; DCIM copy
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
```

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
- The XMP watcher skips `.git/` during its initial walk. Deliberate: watching
  git internals would leak fsnotify watches on every commit the watcher itself
  makes.
- `ErrIngestAlreadyRunning` / `ErrOutgestAlreadyRunning` error strings match
  the wire JSON `"error"` field exactly (`"ingest_already_running"` /
  `"outgest_already_running"`). The IPC handler uses `err.Error()` directly as
  the JSON value — no mapping table.
- The launchd plist is re-generated on every `framelogd install`, not cached.
  This ensures the plist always references the current executable path even if
  you moved or rebuilt the binary.

## What's not done yet (Phase 5/6)

- **FL-404 socket migration:** Swift app buttons still use trigger files.
  Once migrated, both `.ingest_trigger` and `.outgest_trigger` can be retired
  from the protocol.
- **FL-501 runbook execution:** `docs/PHASE5_RUNBOOK.md` is written; manual
  steps (SD card, Lightroom, launchd install) are pending execution.
- **FL-601–604:** Codesigning, version number end-to-end check, installer DMG,
  crash/restart policy.
