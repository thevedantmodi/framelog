# Framelog

Photo ingestion, versioning, and sync pipeline for Mac. Insert an SD card and photos are automatically imported, renamed, deduplicated, and versioned. Edits made in Lightroom Classic are committed to git via XMP sidecars.

## How it works

1. Insert SD card → launchd fires `on_sd_mount.sh`
2. Photos copied to `~/Photos/inbox/`, then ingested to `originals/YYYY/MM/DD/YYYYMMDD_HHMMSS_<hash8>.ext`
3. XMP sidecar written alongside each file, import recorded in `catalog.db`
4. Lightroom picks up new files via Synchronize Folder
5. Edits in Lightroom are auto-committed to git by the background watcher
6. Export finished edits to `~/Photos/processed/` → outgest moves them to `YYYY/MM/` subfolders

---

## Requirements

- macOS (tested on Sequoia)
- [Homebrew](https://brew.sh)
- [uv](https://docs.astral.sh/uv/getting-started/installation/) — Python package manager
- [exiftool](https://exiftool.org) — reads EXIF metadata
- [Lightroom Classic](https://www.adobe.com/products/photoshop-lightroom-classic.html)
- Git

---

## Installation

### 1. System dependencies

```bash
brew install exiftool git
```

Install uv:

```bash
curl -LsSf https://astral.sh/uv/install.sh | sh
```

### 2. Clone the repo

```bash
git clone <your-repo-url> ~/dev/framelog
cd ~/dev/framelog
```

### 3. Install Python dependencies

```bash
uv sync
```

### 4. Create the photo library directories

```bash
mkdir -p ~/Photos/inbox ~/Photos/originals ~/Photos/processed
```

### 5. Initialize the git repo for XMP versioning

```bash
cd ~/Photos/originals
git init
git remote add origin git@github.com:you/framelog-xmp.git
```

Create `~/Photos/originals/.gitignore` to track only XMP files:

```gitignore
*.raf
*.RAF
*.cr3
*.CR3
*.heic
*.HEIC
*.jpg
*.jpeg
*.JPG
*.mp4
*.mov
!*.xmp
```

### 6. Edit the launchd plists

The plists hardcode paths. Open each file in `launchd/` and replace `/Users/vedantmodi` with your home directory:

```bash
sed -i '' 's|/Users/vedantmodi|'"$HOME"'|g' launchd/*.plist scripts/on_sd_mount.sh
```

### 7. Load launchd jobs

**SD card trigger** — fires `on_sd_mount.sh` when any volume mounts:

```bash
cp launchd/com.framelog.sdcard.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.framelog.sdcard.plist
```

**XMP watcher** — commits Lightroom edits to git while Lightroom is open:

```bash
cp launchd/com.framelog.watcher.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.framelog.watcher.plist
```

**Outgest** — organizes flat exports into `YYYY/MM/` subfolders:

```bash
cp launchd/com.framelog.outgest.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.framelog.outgest.plist
```

### 8. Verify jobs loaded

```bash
launchctl list | grep framelog
```

Expected output — exit code should be `0` for sdcard/outgest and a PID for watcher:

```
-       0       com.framelog.sdcard
10462   0       com.framelog.watcher
-       0       com.framelog.outgest
```

---

## Lightroom Classic setup

1. **Add originals as source**
   File → Add Folder to Catalog → `~/Photos/originals/`

2. **Enable auto XMP writes** (required for watcher to work)
   Lightroom → Settings → Catalog Settings → Metadata tab
   Check **Automatically write changes into XMP**

3. **Disable Lightroom's import dialog**
   Preferences → Import → uncheck **Show import dialog when a memory card is detected**
   Never let Lightroom move or rename files — framelog owns all file operations.

4. **Sync folder after each ingest**
   Right-click `originals/` in Library panel → **Synchronize Folder**
   This makes Lightroom pick up newly imported files without re-importing them.

5. **Export path**
   In the Export dialog, set Export Location to:
   - Export To: Specific folder
   - Folder: `~/Photos/processed/`
   - Do not use subfolders — outgest handles YYYY/MM organization automatically

6. **End of edit session**
   Hit **Cmd+S** before closing Lightroom to flush XMP to disk so the watcher commits everything.

---

## Day-to-day workflow

### Importing from SD card

1. Insert SD card — import runs automatically
2. Watch progress: `tail -f ~/Photos/framelog.log`
3. When done, open Lightroom and Synchronize Folder

### Editing

1. Edit photos in Lightroom as normal
2. The watcher commits XMP changes to git automatically (within ~10 seconds of Cmd+S)
3. Check commits: `git -C ~/Photos/originals log --oneline -5`

### Exporting

1. Export finished photos flat to `~/Photos/processed/`
2. Outgest fires automatically and moves them to `~/Photos/processed/YYYY/MM/`

---

## Backup

Backup via rclone is implemented in `on_sd_mount.sh` but disabled by default. To enable, uncomment the backup drive check and rclone sync blocks in `scripts/on_sd_mount.sh` and set your backup drive path:

```bash
BACKUP_PATH="/Volumes/YourDriveName"
```

---

## Monitoring

```bash
# Live log
tail -f ~/Photos/framelog.log

# Check all jobs are running
launchctl list | grep framelog

# Recent imports
sqlite3 ~/Photos/catalog.db "SELECT import_timestamp, COUNT(*) FROM photos GROUP BY date(import_timestamp) ORDER BY import_timestamp DESC LIMIT 10;"

# Recent git commits
git -C ~/Photos/originals log --oneline -10
```

---

## File structure

```
~/Photos/
├── inbox/              ← temporary landing zone (cleared after import)
├── originals/
│   └── YYYY/MM/DD/
│       └── YYYYMMDD_HHMMSS_<hash8>.ext   ← canonical filename
│       └── YYYYMMDD_HHMMSS_<hash8>.xmp   ← XMP sidecar (git tracked)
├── processed/          ← Lightroom exports
│   └── YYYY/MM/
└── catalog.db          ← SQLite index
```

---

## Running tests

```bash
uv run pytest tests/
```
