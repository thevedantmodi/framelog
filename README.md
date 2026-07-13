# Framelog

Framelog is a digital asset management (DAM) tool that 
watches for an SD card, imports your photos, tracks edit history in git,
and organizes exports — all automatically.

---

## Install

### Option A: Homebrew (recommended)

```bash
brew tap thevedantmodi/framelog
brew install --cask --no-quarantine framelog
```

Open **Framelog** from Launchpad or Spotlight. Click the menu bar icon (a
small lens) → **Install Core…**. The daemon (background agent) is now running and
will start automatically every time you log in.

### Option B: Download the DMG

1. Go to the [Releases](https://github.com/thevedantmodi/framelog/releases)
   page and download the latest `Framelog-<version>.dmg`.
2. Open the DMG and drag `Framelog.app` to `Applications`.
3. Open `Framelog.app` from `Applications`. It appears in the menu bar.
4. Click the menu bar icon → **Install Core…**.

> **First-launch warning:** Framelog isn't signed with an Apple Developer
> ID (that costs $99/year, and this is a personal project). macOS will warn
> you the first time you open it. Right-click the app → **Open** → **Open**
> in the dialog that follows. You only need to do this once.

---

## Using it

Once installed, Framelog runs itself:

1. **Insert an SD card.** Framelog notices it within a couple of seconds,
   copies everything from `DCIM/` into a staging folder, then imports it:
   files are hashed (to catch duplicates), renamed, and sorted into
   `~/Photos/originals/YYYY/MM/DD/`.
    - Each file is named as `YYYYMMDD_HHMMSS_[8letterhash]`.
2. **Edit in Lightroom.** Point Lightroom at `~/Photos/originals/` as your
   catalog folder. Edit as normal.
3. **Your edits are saved to history automatically.** A few seconds after
   you save an edit in Lightroom, Framelog commits it to a local git history
   inside `originals/`. If you ever need to undo an edit or see what changed,
   that history is there.
4. **Export from Lightroom into `~/Photos/processed/`.** Framelog notices
   the export and files it into `processed/YYYY/MM/`.
5. **(Optional) Back up automatically.** If you've set a backup drive (see
   below), every import is copied there too.

You generally don't need to open the menu bar app at all — but here's what's
in it:

| Menu item | What it does |
|---|---|
| Status line | Photo count and how long ago the last import happened. See below for what the different messages mean. |
| Backup line | Whether a backup drive is configured and currently connected. |
| ⚠ warnings | Shown if an optional feature (SD card detection, backups, etc.) is unavailable — see [Optional features](#optional-features-and-what-happens-without-them) below. |
| Launch at Login | Whether the menu bar app itself opens automatically at login. |
| Pause/Resume Framelog | Temporarily stop all automatic imports/exports (e.g. before you unplug a drive). Nothing is lost — pending work runs once you resume. |
| Run Ingest Now | Manually trigger an import, without waiting for a card insert. |
| Run Outgest Now | Manually trigger filing of anything sitting in `processed/`. |
| Install Core… | Installs/reinstalls the background daemon. Use this after updating the app. |
| Restart Core | Restarts the daemon without a full reinstall — use this if it seems stuck. |
| Set Git Remote… | Point the `originals/` history at a remote git repo (e.g. GitHub) so your edit history is backed up off your Mac too. |
| Open Log File | Opens the daemon's log for troubleshooting. |
| Set Backup Drive… | Choose an external drive to automatically copy `originals/` to after every import. |
| Run Setup | Re-registers login item + notification permissions, if either got reset. |
| Quit Framelog | Quits the menu bar app only. The background daemon keeps running — your imports/exports still happen even with the menu bar app closed. |

### What the status line means

| Status line | Meaning |
|---|---|
| "Install Core to get started" | The daemon has never been installed. Click **Install Core…**. |
| "Core restarting…" | The daemon crashed and macOS is automatically restarting it. If this doesn't clear within a few seconds, open the log file. |
| "No photos imported yet" | Daemon is running, nothing has come through yet. |
| "N photos · last import: X ago" | Normal operation. |

### Optional features and what happens without them

Two things (`exiftool`, `git`) are required — the daemon won't start
without them. Everything else degrades gracefully; you'll see a warning (⚠)
in the menu bar instead of a silent failure:

| Feature | Needs | If missing |
|---|---|---|
| SD card auto-detection + backups | `diskutil`, `rclone` | SD card watching and backups are disabled. You can still drop files into `~/Photos/inbox/` manually and click **Run Ingest Now**. |
| Push-on-AC-power-only | `pmset` | Edit history is pushed to your git remote regardless of power state. |
| "Don't push while Lightroom is open" | `pgrep` | Edit history is pushed even while Lightroom is open. |

If you installed via the Homebrew cask, `exiftool`, `git`, and `rclone` are
pulled in automatically as dependencies — nothing to install by hand.
If you installed the DMG directly, get them via Homebrew:
`brew install exiftool git rclone`.

---

## Where your files live

```
~/Photos/
├── inbox/                  ← SD card staging area, cleared after each import
│   ├── duplicates/         ← files already in your library — parked here, not re-imported
│   └── failed/             ← files that couldn't be imported after 3 tries (corrupt/unreadable)
├── originals/YYYY/MM/DD/   ← your imported photos + edit-history sidecar files
├── processed/YYYY/MM/      ← your Lightroom exports, auto-filed by date
├── catalog.db              ← Framelog's internal database — don't edit by hand
└── framelog.log            ← plain-text log, useful for troubleshooting
```

`originals/` is a git repository, but it only tracks the small edit-history
files (XMP sidecars), not your actual photos — so it stays small even with a
huge photo library.

---

## Troubleshooting

**"Core restarting…" won't go away.**
Click **Open Log File** and look at the last few lines. Also check
`~/Library/Logs/Framelog/crash.log` — if it has anything in it, that's the
crash reason. Try **Restart Core**; if that doesn't help, **Install Core…**
to reinstall the launchd job from scratch.

**Nothing happens when I insert an SD card.**
Check for the ⚠ warning about SD card detection in the menu — you may be
missing `diskutil` or `rclone`. You can still import manually: copy the
photos into `~/Photos/inbox/` and click **Run Ingest Now**.

**A photo didn't import.**
Check `~/Photos/inbox/duplicates/` (it's probably already in your library)
and `~/Photos/inbox/failed/` (it couldn't be read after 3 attempts — the file
may be corrupt).

**Backup drive shows disconnected but it's plugged in.**
Reopen **Set Backup Drive…** and re-select it — the daemon needs the exact
mount path, which can change between plug-ins for some drives.

**I want to check things from the terminal instead.**
```bash
tail -f ~/Photos/framelog.log
echo '{"command":"status"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
```

---

## Uninstalling

Click the menu bar icon and look for the daemon's install location, or run:

```bash
/Applications/Framelog.app/Contents/MacOS/framelogd uninstall
```

then drag `Framelog.app` to the Trash. Your photos in `~/Photos/` are never
touched by uninstalling.

---

## For developers

The sections below are for people building or contributing to Framelog
itself — not needed to just use the app.

### Build from source

```bash
git clone https://github.com/thevedantmodi/framelog
cd framelog
make release          # builds Go + Swift, bundles framelogd into .app, creates DMG
# → open build/Framelog-<version>.dmg
```

```bash
make build        # build Go binary + Swift app
make build-go     # Go only
make build-swift  # Swift only
make test         # Go tests (race detector) + Xcode tests
make sha          # print sha256 of the DMG (for Homebrew cask)
make clean        # remove build artefacts
```

Run the daemon directly (dev/testing):

```bash
cd core
go build -o framelogd ./cmd/framelogd
./framelogd run          # foreground
./framelogd install      # as a launchd agent
./framelogd uninstall
```

Reset a dev/test environment:

```bash
cd core && make reset   # removes ~/Photos/{inbox,originals,processed}, catalog.db, log, triggers
```

### Cutting a release

```bash
echo "0.2.0" > VERSION
git add VERSION && git commit -m "chore: bump version to 0.2.0"
git tag v0.2.0 && git push origin main && git push origin v0.2.0
```

Pushing the tag triggers CI, which builds the DMG and creates a GitHub
Release automatically. See `docs/RELEASE_RUNBOOK.md` for the full process,
including the Homebrew tap update.

### Architecture

```
framelogd (Go daemon)
├── SD card watcher      polls /Volumes every 2s; copies DCIM → inbox/ via rclone on mount
├── ingest               hash → dedup/quarantine → rename → originals/ → catalog.db → git commit → backup
├── XMP watcher          fsnotify on originals/; debounce → git commit → push (gated on AC power + Lightroom closed)
│   └── DNG handling     exiftool -xmp -b extracts embedded XMP to .xmp sidecar
├── outgest watcher      fsnotify on processed/; debounce → organise into YYYY/MM/
├── backup               rclone copy originals/ → configured backup path, after ingest
├── trigger poller       polls .ingest_trigger / .outgest_trigger every 2s (v1 IPC)
└── IPC server           Unix socket (v2 IPC): ingest_now / outgest_now / status / pause / resume / set_backup_path

Framelog.app (Swift menu bar)
├── polls catalog.db read-only every 15s (photo count, last import)
├── pings Unix socket to detect core alive vs. crashed vs. never installed
├── fires UserNotifications on import delta
└── touches trigger files for Run Ingest / Run Outgest buttons (socket migration pending, see docs/ROADMAP.md)
```

See `docs/PROTOCOL.md` for the frozen core↔frontend contract (IPC shapes,
DB schema, log format) and `docs/ROADMAP.md` for what's built vs. planned.

### Development

```bash
cd core && go test ./... -race     # Go tests
cd .. && make test                 # + Xcode tests
cd core && make reset              # reset test environment
```

No test requires `exiftool`, `diskutil`, `pmset`, `pgrep`, or `rclone` on
`PATH` — all external binaries use injectable paths, and tests substitute
fake shell scripts. See `CLAUDE.md` for the conventions this repo follows.
