# Framelog

Framelog is a Mac menu bar app that automates the photo import pipeline from SD card and iPhone into a version-controlled archive. Insert a card and photos are copied to a dated folder structure, renamed by capture time, deduplicated by hash, and indexed in a local SQLite catalog. Edits made in Lightroom Classic are automatically committed to git via XMP sidecars — so your develop settings have a full history without touching the raw files.

The app lives in the menu bar. It watches for SD cards in the background, tracks Lightroom for XMP changes, and organizes finished exports. You don't need to think about it.

---

## What it does

**On SD card insert:**
Photos are copied from the card's DCIM folder to `~/Photos/inbox/`, then ingested into `~/Photos/originals/YYYY/MM/DD/` with a canonical filename based on capture time and a SHA-256 hash prefix. Duplicates are silently skipped. A `.xmp` sidecar is written next to each file and the import is recorded in `catalog.db`.

**While Lightroom is open:**
The app watches `originals/` for XMP changes. When you hit Cmd+S or Lightroom flushes settings, the changed files are debounced and committed to git automatically. On AC power, commits are pushed to the remote.

**After exporting:**
Drop finished exports flat into `~/Photos/processed/`. The app detects new files and moves them into `YYYY/MM/` subfolders based on capture date.

---

## Photo library layout

```
~/Photos/
├── inbox/                          ← temporary landing zone (cleared after import)
├── originals/
│   └── YYYY/MM/DD/
│       ├── YYYYMMDD_HHMMSS_<hash8>.raf   ← canonical raw file
│       └── YYYYMMDD_HHMMSS_<hash8>.xmp   ← XMP sidecar (git tracked)
├── processed/
│   └── YYYY/MM/                    ← outgest organizes exports here
└── catalog.db                      ← SQLite index
```

---

## Requirements

- macOS Sequoia or later
- [exiftool](https://exiftool.org) — reads EXIF metadata from RAF, HEIC, JPG
- Lightroom Classic (optional — XMP watcher only activates when it's running)
- A GitHub repo for XMP version history (free, files are tiny)

---

## Installation

### 1. Install exiftool

```bash
brew install exiftool
```

### 2. Build or download the app

**From source:**

```bash
git clone <repo-url> ~/dev/framelog
cd ~/dev/framelog
make release          # builds dist/Framelog.app and packages a DMG
```

**From DMG:**
Open `Framelog-1.0.0.dmg` and drag `Framelog.app` to Applications.

### 3. Launch the app

Open `Framelog.app`. On first launch it will:
- Create `~/Photos/inbox/`, `originals/`, and `processed/`
- Initialize a git repo in `originals/` with the correct `.gitignore`
- Install and load a launchd agent so the app runs at login and stays running
- Install and load the SD card trigger agent
- Show a notification when setup is done

The menu bar icon appears as 📷.

### 4. Set up XMP versioning (optional but recommended)

Click **Set Git Remote…** in the menu bar and paste your GitHub SSH URL. That's it — the repo and `.gitignore` are already initialized.

If you don't have a remote repo yet, create one at [github.com/new](https://github.com/new) (private, empty, no README).

---

## Lightroom Classic setup

1. **Add originals as source folder**
   File → Add Folder to Catalog → `~/Photos/originals/`

2. **Enable auto XMP writes**
   Lightroom → Settings → Catalog Settings → Metadata tab
   Check **Automatically write changes into XMP**

3. **Disable Lightroom's import dialog**
   Preferences → Import → uncheck **Show import dialog when a memory card is detected**
   Framelog owns all file operations — never let Lightroom move or rename files.

4. **Sync folder after each ingest**
   Right-click `originals/` in the Library panel → **Synchronize Folder**
   This picks up newly imported files without triggering a full re-import.

5. **Set the export path**
   Export Location → Specific folder → `~/Photos/processed/`
   Do not use subfolders — outgest handles `YYYY/MM/` organization automatically.

6. **End of each session**
   Hit **Cmd+S** before closing Lightroom to flush XMP to disk before the watcher commits.

---

## Day-to-day workflow

### Importing from an SD card

1. Insert the card — import starts automatically within a few seconds
2. The menu bar icon changes to ⏳ while running
3. A notification appears when done: `N imported · N skipped · N failed`
4. In Lightroom, right-click `originals/` → **Synchronize Folder** to see new photos

You can also trigger import manually from the menu bar: **Run Ingest Now**.

### Editing in Lightroom

Edit as normal. XMP changes are committed to git automatically — you don't need to do anything. The commit message includes the timestamp and file count.

To check recent commits:

```bash
git -C ~/Photos/originals log --oneline -10
```

### Exporting finished edits

Export flat to `~/Photos/processed/`. Within a few seconds, outgest moves files into `~/Photos/processed/YYYY/MM/` and shows a notification.

You can also trigger outgest manually: **Run Outgest Now**.

### Checking status

The **Status** item in the menu shows total photo count and last import time. For more detail, open the log:

```
~/Photos/framelog.log
```

Or live:

```bash
tail -f ~/Photos/framelog.log
```

---

## Backup

The SD card trigger script (`on_sd_mount.sh`) has commented-out blocks for rclone backup. To enable:

1. Connect your backup drive
2. Open `~/.framelog_on_sd_mount.sh`
3. Set `BACKUP_PATH` to your drive's mount path and uncomment the backup drive check and rclone blocks

Backup strategy:
| Copy | Where | Method |
|---|---|---|
| Primary | Main machine | — |
| Local backup | External drive | rclone on SD card insert |
| XMP edit history | GitHub | auto-push on AC power |

---

## Troubleshooting

**Import didn't start after inserting SD card**
Check that the card has a `DCIM` folder and is recognized as removable media:
```bash
diskutil info /Volumes/<CardName> | grep "Removable Media"
```
Check the log: `tail -20 ~/Photos/framelog.log`

**XMP changes not being committed**
Verify Lightroom has "Automatically write changes into XMP" enabled. Check that the app is running (📷 icon in menu bar). Commits are debounced — changes appear in git ~10 seconds after the last Cmd+S.

**Duplicate photos**
Deduplication is by SHA-256 hash. If a photo was already imported it will be logged as `skipped` and the source file left untouched.

---

## Development

### Running from source

```bash
uv run python -m framelog
```

### Running tests

```bash
uv run pytest tests/
```

### Building the app bundle

```bash
make build    # builds dist/Framelog.app
make dmg      # signs and packages a DMG
make release  # build + dmg in one step
make clean    # remove build artifacts
```

The build uses py2app. A separate `.venv-build/` virtualenv is used for the build toolchain to avoid conflicts with the dev environment.

### Project layout

```
src/framelog/
├── __main__.py     ← entry point
├── menubar.py      ← menu bar app, XMP watcher, outgest watcher, ingest poller
├── ingest.py       ← import pipeline
├── outgest.py      ← export organizer
├── firstrun.py     ← first-launch setup, launchd agent install
├── config.py       ← all paths and constants
├── db.py           ← SQLite catalog
├── exif.py         ← exiftool wrapper
├── hasher.py       ← SHA-256 file hashing
├── xmp.py          ← XMP sidecar writer
├── git.py          ← git commit/push
└── on_sd_mount.sh  ← SD card trigger script (bundled into app)
```
