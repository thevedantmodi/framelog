# Framelog

A macOS photo pipeline: a headless Go daemon (`framelogd`) and a Swift menu bar app
(`Framelog.app`). Insert an SD card — photos are imported, deduplicated, git-versioned,
and Lightroom edits are committed automatically. No Python. No shell scripts.

---

## How it works

1. **SD card inserted** → `framelogd` detects it, copies DCIM to `~/Photos/inbox/`
2. **Ingest** → files are hashed, renamed `<date>_<hash8>.ext`, moved to
   `originals/YYYY/MM/DD/`, recorded in `catalog.db`
3. **Lightroom** → open `~/Photos/originals/` as a catalog; edit freely
4. **XMP watcher** → when Lightroom saves edits (DNG: embedded XMP extracted to sidecar;
   other formats: `.xmp` sidecar written by Lightroom), debounced git commit in `originals/`
5. **Outgest** → Lightroom exports land in `~/Photos/processed/`; `framelogd` organises
   them into `processed/YYYY/MM/`
6. **Backup** → after ingest, `rclone copy` syncs `originals/` to `$FRAMELOG_BACKUP_PATH`

The menu bar app shows photo count, last import time, and lets you trigger ingest/outgest
manually. The daemon runs as a `launchd` KeepAlive agent — it restarts automatically on
crash and starts at login.

---

## Install

### From DMG (recommended)

1. Download `Framelog-<version>.dmg` from the [Releases](https://github.com/thevedantmodi/framelog/releases) page
2. Drag `Framelog.app` to `/Applications`
3. Open `Framelog.app` — it appears in the menu bar
4. Click the menu bar icon → **Install Core…** — this installs `framelogd` as a launchd agent

### Via Homebrew (tap)

```bash
brew tap thevedantmodi/framelog
brew install --cask --no-quarantine framelog
```

Then open `Framelog.app` and click **Install Core…**.

> **Note:** `--no-quarantine` is required because the app is not yet notarized with Apple.
> This is safe for software you build or download from a trusted source.

### From source

```bash
git clone https://github.com/thevedantmodi/framelog
cd framelog
make release          # builds Go + Swift, bundles framelogd into .app, creates DMG
# → open build/Framelog-<version>.dmg
```

---

## Menu bar

| Item | What it does |
|------|-------------|
| Status line | Photo count + time since last import. Shows "Core restarting…" if the daemon crashed (launchd is recovering), "Install Core to get started" if never set up. |
| Launch at Login | Registers/unregisters the menu bar app itself as a login item. |
| Run Ingest Now | Triggers ingest immediately (touches `.ingest_trigger`). |
| Run Outgest Now | Triggers outgest immediately (touches `.outgest_trigger`). |
| Install Core… | Runs `framelogd install` from the bundled binary — writes the launchd plist and starts the daemon. |
| Open Log File | Opens `~/Photos/framelog.log` in your default viewer. |
| Run Setup | Re-runs login-item registration and notification permission request. |
| Quit Framelog | Quits the menu bar app. `framelogd` keeps running. |

---

## Required and optional binaries

| Binary     | Required? | If absent |
|------------|-----------|-----------|
| `exiftool` | Yes | Daemon refuses to start |
| `git`      | Yes | Daemon refuses to start |
| `pmset`    | No  | Push not gated on AC power (always pushes) |
| `pgrep`    | No  | Lightroom-running check skipped (always pushes after debounce) |
| `diskutil` | No  | SD card watcher disabled |
| `rclone`   | No  | Backup disabled |

Check `~/Photos/framelog.log` after first start to see which capabilities are active.

---

## Day-to-day

**Build everything:**
```bash
make build
```

**Run daemon in the foreground (dev/testing):**
```bash
./core/framelogd run
```

**Install as launchd agent:**
```bash
./core/framelogd install
```

**Check status:**
```bash
echo '{"command":"status"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
```

**Tail logs:**
```bash
tail -f ~/Photos/framelog.log
```

**Uninstall:**
```bash
./core/framelogd uninstall
```

**Reset for testing:**
```bash
cd core && make reset   # removes ~/Photos/{inbox,originals,processed}, catalog.db, log, triggers
```

---

## Build system

The root `Makefile` drives both binaries from the single `VERSION` file:

```bash
make build        # build Go binary + Swift app
make build-go     # Go only
make build-swift  # Swift only
make test         # Go tests (race detector) + Xcode tests
make release      # full release: build + bundle framelogd into .app + create DMG
make sha          # print sha256 of the DMG (for Homebrew cask)
make clean        # remove build artefacts
```

**Bump version:**
```bash
echo "0.2.0" > VERSION
make release
```

---

## File layout on disk

```
~/Photos/
├── inbox/                ← SD card landing zone, cleared after import
├── originals/YYYY/MM/DD/ ← imported files + .xmp sidecars; git-tracked (XMP only)
├── processed/YYYY/MM/    ← Lightroom exports, organised by outgest
├── catalog.db            ← SQLite; core writes, frontend reads read-only
└── framelog.log          ← structured log (TIMESTAMP [PREFIX] message)

~/Library/LaunchAgents/
└── com.framelog.core.plist     ← written by `framelogd install`

~/Library/Application Support/Framelog/
└── framelog.sock               ← v2 IPC socket (created at runtime)

~/Library/Logs/Framelog/
└── crash.log                   ← launchd stdout/stderr (empty in normal operation)
```

`inbox/`, `originals/`, `processed/`, and `originals/.git` are created automatically
on first run — no manual `git init` needed.

The `originals/` git repo tracks only `.xmp` sidecar files (`.gitignore` ignores
everything else). Large RAW files stay on disk but are not versioned.

---

## Architecture

```
framelogd (Go daemon)
├── SD card watcher      polls /Volumes every 2s; copies DCIM → inbox/ on mount
├── ingest               hash → rename → originals/ → catalog.db → git commit → backup
├── XMP watcher          fsnotify on originals/; 10s debounce → git commit → push
│   └── DNG handling     exiftool -xmp -b extracts embedded XMP to .xmp sidecar
├── outgest watcher      fsnotify on processed/; debounce → organise into YYYY/MM/
├── backup               rclone copy originals/ → $FRAMELOG_BACKUP_PATH after ingest
├── trigger poller       polls .ingest_trigger / .outgest_trigger every 2s (v1 IPC)
└── IPC server           Unix socket (v2 IPC): ingest_now / outgest_now / status

Framelog.app (Swift menu bar)
├── polls catalog.db read-only every 15s (photo count, last import)
├── pings Unix socket to detect core alive vs. crashed vs. never installed
├── fires UserNotifications on import delta
└── touches trigger files for Run Ingest / Run Outgest buttons
```

See `docs/PROTOCOL.md` for the frozen core↔frontend contract.

---

## IPC reference

**Socket:** `~/Library/Application Support/Framelog/framelog.sock`

```bash
echo '{"command":"status"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
echo '{"command":"ingest_now"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
echo '{"command":"outgest_now"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
```

Full shapes in `docs/PROTOCOL.md §3`.

---

## Development

```bash
# Go tests (no external binaries required except git):
cd core && go test ./... -race

# Xcode tests:
cd .. && make test

# Reset test environment:
cd core && make reset
```

No test requires `exiftool`, `diskutil`, `pmset`, `pgrep`, or `rclone` on PATH — all
external binaries use injectable paths and tests substitute fake shell scripts.

---

## Phase status

**Phase 1–4: complete.**

**Phase 5 (migration/cutover):**
- [x] FL-501 — cold-start verification
- [x] FL-502/503 — real-hardware SD card + Lightroom XMP pipeline tested end-to-end
- [x] FL-504 — docs rewritten against actual architecture

**Phase 6 (distribution):**
- [ ] FL-601 — codesigning + notarization (requires $99 Apple Developer account; deferred)
- [x] FL-602 — single `VERSION` file drives `framelogd --version` and `CFBundleShortVersionString`
- [x] FL-603 — `framelogd` bundled in `Framelog.app/Contents/MacOS/`; `make release` produces DMG; Install Core button
- [x] FL-604 — four-state status display; socket ping distinguishes crash/never-installed/running

**Distribution:**
- Homebrew tap: `brew tap thevedantmodi/framelog` (see `homebrew/framelog.rb`)
- GitHub Releases: DMG built by `make release`

---

## Decommissioned

The Python version (`menubar.py`, `on_sd_mount.sh`) and its launchd jobs
(`com.framelog.sdcard`, `com.framelog.app`) have been fully replaced.
