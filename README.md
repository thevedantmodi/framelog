# Framelog

A macOS photo pipeline running as a headless Go daemon (`framelogd`) and a
Swift menu bar app (`Framelog.app`). No Python. No separate shell scripts.

- **Go core** (`core/`) owns everything pipeline-related: SD card detection,
  ingest, XMP/git, outgest, backup, and IPC. Runs as a `launchd` KeepAlive
  agent.
- **Swift frontend** (`menubar/`) is a thin menu bar shell: reads
  `catalog.db` for status, tails `framelog.log` for the log viewer, and sends
  commands to the core over a Unix domain socket.

See `docs/PROTOCOL.md` for the frozen core↔frontend contract and
`docs/ROADMAP.md` for the full ticket backlog.

---

## Architecture

```
framelogd (Go binary)
├── SD card watcher     → ingest on mount
├── trigger-file poller → ingest/outgest on touch (v1 IPC, FL-301)
├── XMP watcher         → git commit/push on Lightroom edit
├── outgest watcher     → organise processed/ on Lightroom export
├── backup              → rclone copy originals/ to BACKUP_PATH after ingest
└── IPC server          → Unix socket at ~/Library/Application Support/Framelog/framelog.sock

Framelog.app (Swift menu bar)
├── reads catalog.db read-only (photo count, last import)
├── tails ~/Photos/framelog.log (log viewer)
├── touches ~/Photos/.ingest_trigger / .outgest_trigger (v1 IPC buttons)
│   └── planned: migrate to socket ("ingest_now"/"outgest_now") — see FL-404
└── polls socket every 15s for status (photo count, backup drive, running state)
```

---

## `framelogd` subcommands

```bash
framelogd run        # start the daemon (what launchd runs; also use for manual foreground testing)
framelogd install    # write ~/Library/LaunchAgents/com.framelog.core.plist and bootstrap it
framelogd uninstall  # bootout the agent and remove the plist
framelogd --version  # print the build version
```

`install` and `uninstall` are explicit subcommands. They do not fire as a side
effect of `framelogd run` or `go run` — auto-installing a launchd agent during
development would be a footgun.

### Build

```bash
cd core
go build -ldflags "-X main.Version=$(cat ../VERSION)" -o framelogd ./cmd/framelogd
```

For development without version stamping:
```bash
go build -o framelogd ./cmd/framelogd
```

### Test

```bash
go test ./... -race
```

No test requires `exiftool`, `diskutil`, `pmset`, `pgrep`, or `rclone` to be
installed — all external binaries use injectable paths so tests substitute fake
shell scripts. `git` is required on PATH for the cold-start test
(`TestColdStart_DirectoriesAndDB`), which verifies `initWorkspace` creates
`originals/.git` from scratch.

---

## IPC — talking to the daemon

### v2 socket (primary)

Path: `~/Library/Application Support/Framelog/framelog.sock`

Line-delimited JSON, one connection per request. Test with netcat:

```bash
echo '{"command":"status"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
# → {"protocol_version":1,"ok":true,"ingest_running":false,"outgest_running":false,
#    "photo_count":4213,"last_import":"2026-06-20T14:02:00Z","backup_drive_mounted":true}

echo '{"command":"ingest_now"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
# → {"protocol_version":1,"ok":true,"imported":3,"skipped":1,"failed":0}

echo '{"command":"outgest_now"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
# → {"protocol_version":1,"ok":true,"moved":2,"skipped":0,"failed":0}
```

Full request/response shapes in `docs/PROTOCOL.md §3`.

### v1 trigger files (used by Swift app buttons today)

The Swift "Run Ingest Now" / "Run Outgest Now" buttons currently touch
`~/Photos/.ingest_trigger` and `~/Photos/.outgest_trigger`. The Go core polls
for these every 2 seconds and fires the corresponding pipeline. This will
migrate to the socket once FL-404 is updated — see the follow-up note in
`docs/PROTOCOL.md §2`.

---

## File layout on disk

```
~/Photos/
├── inbox/                ← landing zone, cleared after import
├── originals/YYYY/MM/DD/ ← imported files + .xmp sidecars; git-tracked
├── processed/YYYY/MM/    ← Lightroom exports, organised by outgest
├── catalog.db            ← SQLite; core writes, frontend reads read-only
└── framelog.log          ← structured log (TIMESTAMP [PREFIX] message)

~/Library/LaunchAgents/
└── com.framelog.core.plist     ← written by `framelogd install`

~/Library/Application Support/Framelog/
└── framelog.sock               ← v2 IPC socket (created at runtime)

~/Library/Logs/Framelog/
└── crash.log                   ← launchd stdout/stderr capture (empty in normal operation)
```

`inbox/`, `originals/`, `processed/`, and the `originals/.git` repo are created
automatically on first `framelogd run` — you do not need to create them or run
`git init` manually.

---

## Required and optional binaries

| Binary     | Required? | If absent                                   |
|------------|-----------|---------------------------------------------|
| `exiftool` | Yes       | Daemon refuses to start                     |
| `git`      | Yes       | Daemon refuses to start                     |
| `pmset`    | No        | Push not gated on AC power (always pushes)  |
| `pgrep`    | No        | Lightroom-running check skipped (always push after debounce) |
| `diskutil` | No        | SD card watcher disabled                    |
| `rclone`   | No        | Backup disabled                             |

The startup log reports the resolved path for each binary (or the degradation
note if absent). Check `~/Photos/framelog.log` after first start to confirm
which capabilities are active.

---

## Phase status

Phase 1 (Go primitives), complete:

- [x] FL-101 — `core/config`
- [x] FL-102 — `core/hasher`
- [x] FL-103 — `core/db` (InitDB, InsertPhoto, HashExists, UpdateStatus, PhotoCount, LastImport)
- [x] FL-104 — `core/exif` (injectable binary path)
- [x] FL-105 — `core/xmp` (WriteXMP, xpacket wrapper, dc:subject keyword bag)
- [x] FL-106 — `core/gitops` (FindGit, Commit, FindPmset, IsOnACPower, Push)

Phase 2 (orchestration), complete:

- [x] FL-206 — `core/logging`
- [x] FL-201 — `core/ingest` (Pipeline, ImportFile copy-before-delete, RunIngest, concurrency guard, backup call)
- [x] FL-202 — `core/outgest` (Pipeline, OrganizeFile, RunOutgest, UpdateStatusByHashPrefix)
- [x] FL-203 — `core/sdcard` (FindDiskutil, IsRemovableMedia, HasDCIM, FindSDCard, CopyDCIM, Watcher)
- [x] FL-204 — `core/xmpwatcher` (FindPgrep, IsLightroomRunning, Watcher; debounce+commit+push gated on AC and LR closed)
- [x] FL-205 — `core/outgestwatcher` (Watcher; single-dir non-recursive, debounce)
- [x] FL-207 — `core/backup` (FindRclone, IsDriveMounted, Sync via rclone copy)

Phase 3 (IPC & launchd), complete:

- [x] FL-301 — `core/triggerwatcher` (2s poll; both `.ingest_trigger` and `.outgest_trigger`; remove-before-act)
- [x] FL-302 — `core/ipc` (Unix socket; `ingest_now`, `outgest_now`, `status`; status never blocks on pipeline locks)
- [x] FL-303 — `core/launchd` (FindLaunchctl, GeneratePlist, Install, Uninstall)
- [x] FL-304 — `core/cmd/framelogd` (`run`/`install`/`uninstall`/`--version`; testable `runConfig`; cold-start and end-to-end socket tests)

Phase 4 (Swift frontend), complete:

- [x] FL-401 — `MenuBarExtra` skeleton, `LSUIElement=YES`, `photo.stack` icon
- [x] FL-402 — `SMAppService.mainApp.register/unregister` login-item toggle
- [x] FL-403 — Read-only SQLite (photo count, last import); log-tail reader; 15s timer; three display states
- [x] FL-404 — "Run Ingest Now" / "Run Outgest Now" (v1 trigger files; socket migration pending — see FL-404 follow-up in PROTOCOL.md)
- [x] FL-405 — `UserNotifications` on import delta
- [x] FL-406 — "Open Log File" / "Run Setup" / "Quit Framelog"

Phase 5 (cutover), in progress:

- [x] FL-501 — Cold-start verification (`TestColdStart_DirectoriesAndDB`); `~/Photos` test data cleared
- [ ] FL-502/503 — Real-hardware runbook written (`docs/PHASE5_RUNBOOK.md`); pending manual execution
- [x] FL-504 — Docs rewritten against actual architecture (this file; `CLAUDE.md` added)

---

## Day-to-day

**Install the daemon:**
```bash
cd ~/dev/framelog/core
go build -ldflags "-X main.Version=$(cat ../VERSION)" -o framelogd ./cmd/framelogd
./framelogd install
```

**Check status:**
```bash
echo '{"command":"status"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
```

**View logs:**
```bash
tail -f ~/Photos/framelog.log
```

**Uninstall:**
```bash
./framelogd uninstall
```

For a clean reinstall from scratch, follow `docs/PHASE5_RUNBOOK.md`.

---

## Decommissioned

The Python version (`menubar.py`, `on_sd_mount.sh`, `~/.framelog/`) and its two
launchd jobs (`com.framelog.sdcard`, `com.framelog.app`) have been replaced.
See `docs/PHASE5_RUNBOOK.md §11` for the decommission steps.
