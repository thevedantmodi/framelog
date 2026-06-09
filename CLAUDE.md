# Framelog

A photo ingestion, versioning, and sync pipeline for Mac. Built around a Fuji camera (RAF files) and iPhone (HEIC/JPG), with Lightroom Classic as the editing app.

---

## Core Philosophy

- **`ingest.py` owns all file operations** — Lightroom never moves or renames files
- **XMP sidecars are the source of truth** — all develop settings live in open, portable files
- **Deduplication via SHA-256 hash** — the same photo can never be imported twice
- **Copy before delete** — during import, always copy to destination before removing from inbox
- **Non-destructive** — originals are never touched after import

---

## Photo Library Structure

```
~/Photos/
├── inbox/              ← temporary landing zone (cleared after import)
├── originals/
│   └── YYYY/MM/DD/
│       └── YYYYMMDD_HHMMSS_<hash8>.ext   ← canonical filename
├── processed/          ← Lightroom exports (full res + web)
│   └── YYYY/MM/
└── catalog.db          ← SQLite index
```

---

## Source Code Structure

```
framelog/
├── README.md
├── config.py              ← all paths and constants
├── ingest.py              ← entry point, orchestrates the pipeline
├── db.py                  ← all SQLite operations
├── exif.py                ← exiftool wrapper
├── hasher.py              ← SHA-256 file hashing
├── xmp.py                 ← XMP sidecar writer
├── git.py                 ← git commit/push operations
├── watchdog.py            ← background XMP change watcher
├── recipes.py             ← edit delta library CLI
├── scripts/
│   └── on_sd_mount.sh     ← bash trigger for SD card mount
├── launchd/
│   └── com.framelog.plist ← Mac auto-trigger config
└── tests/
    ├── test_hasher.py
    ├── test_exif.py
    └── test_db.py
```

---

## Tech Stack

| Tool | Purpose | How used |
|---|---|---|
| Python 3 | Main application language | Written directly |
| Bash | SD card mount trigger | Written directly |
| `exiftool` | Reads EXIF metadata from RAF/HEIC/JPG | Called via Python subprocess |
| `sqlite3` | Local photo catalog database | Python stdlib, no ORM |
| `git` | XMP version history | Called via Python subprocess |
| `rclone` | Syncs originals to backup drive | Called from Bash |
| `launchd` | Auto-triggers on SD card mount | XML plist config |
| `watchdog` (Python lib) | Watches originals/ for XMP changes | Python directly |

---

## Build Order

Build and test each module in this order — never move to the next until the current one works:

1. **`config.py`** — paths and constants, imported by everything else
2. **`hasher.py`** — SHA-256 hashing, test on a single RAF file
3. **`db.py`** — SQLite schema and operations, verify table creates correctly
4. **`exif.py`** — exiftool wrapper, test with a real RAF file
5. **`xmp.py`** — XMP sidecar writer
6. **`ingest.py`** — core pipeline, wires all modules together
7. **`git.py`** — git commit/push, add after ingest is solid
8. **`scripts/on_sd_mount.sh`** — bash trigger + rclone sync
9. **`launchd/com.framelog.plist`** — auto-trigger, add last
10. **`watchdog.py`** — background XMP watcher for Lightroom sessions
11. **`recipes.py`** — edit delta library, build last

---

## Module Specs

### `config.py`
All paths and constants. Every other module imports from here. Nothing is ever hardcoded elsewhere.

```python
INBOX = Path("~/Photos/inbox").expanduser()
ORIGINALS = Path("~/Photos/originals").expanduser()
PROCESSED = Path("~/Photos/processed").expanduser()
DB_PATH = Path("~/Photos/catalog.db").expanduser()
SUPPORTED_EXTENSIONS = {".raf", ".heic", ".jpg", ".jpeg", ".mp4", ".mov"}
DEBOUNCE_SECONDS = 10
```

---

### `hasher.py`
SHA-256 hash a file in chunks — never load the whole file into memory.

```python
def hash_file(path: Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(8192), b""):
            h.update(chunk)
    return h.hexdigest()
```

---

### `db.py`
SQLite operations. Schema:

```sql
CREATE TABLE IF NOT EXISTS photos (
    hash TEXT PRIMARY KEY,
    original_filename TEXT,
    imported_path TEXT,
    camera_model TEXT,
    capture_date TEXT,
    import_timestamp TEXT,
    status TEXT DEFAULT 'raw'
);
```

Status lifecycle: `raw → culled → edited → published`

Key functions:
- `init_db()` — creates table if not exists
- `hash_exists(hash) -> bool` — dedup check
- `insert_photo(record)` — write import record
- `update_status(hash, status)` — update edit status

---

### `exif.py`
Wraps exiftool via subprocess. Returns a dict of metadata.

Key fields to extract:
- `DateTimeOriginal` — capture timestamp (fall back to file mtime if missing)
- `Model` — camera model
- `GPSLatitude`, `GPSLongitude` — location if present

Call: `exiftool -json <path>` and parse the JSON output.

---

### `xmp.py`
Writes a `.xmp` sidecar alongside each imported file. Uses the `crs:` namespace for develop settings and `dc:subject` for custom tags.

Custom tags to include:
- Source device (Fuji / iPhone)
- Import batch ID (timestamp of the ingest run)
- Camera model

---

### `ingest.py`
Entry point. Orchestrates the pipeline.

`import_file(path)` steps:
1. Hash the file
2. Check db — skip if hash exists
3. Read EXIF
4. Build destination path: `originals/YYYY/MM/DD/YYYYMMDD_HHMMSS_<hash[:8]>.ext`
5. Create destination directory if needed
6. **Copy** file to destination (never move directly)
7. Write XMP sidecar
8. Insert record to db
9. **Delete** source from inbox only after all steps succeed

`run_ingest()`:
- Scans inbox recursively for supported extensions
- Calls `import_file()` on each
- Tracks imported / skipped / failed counts
- Prints summary
- Calls `git_commit()` after completion

---

### `git.py`
Git operations on the `originals/` repo.

`git_commit(message)`:
- Runs `git -C originals/ add -A`
- Checks for staged changes via `git status --porcelain`
- Commits only if changes exist
- Auto message format: `ingest: 2026-05-28 Fuji + iPhone (12 photos)`

`git_push()`:
- Only runs when on AC power
- Check AC power on Mac: `pmset -g batt` → look for "AC Power"

---

### `watchdog.py`
Background process — runs on login via launchd, dormant when Lightroom is closed.

Behaviour:
- Only active while Lightroom process is running (check via `psutil`)
- Watches `originals/` recursively for `.xmp` file modifications
- Uses FSEvents (Mac native, not polling) via the `watchdog` Python library
- Debounces — waits `DEBOUNCE_SECONDS` of quiet before committing
- Commits only changed XMP files
- Pushes to GitHub only on AC power
- Final commit + push when Lightroom quits regardless of power state
- Logs all activity to `~/Photos/framelog.log`

Auto commit message format: `auto: xmp changes 2026-05-28 14:32 (3 files)`

---

### `recipes.py`
CLI tool for managing named edit style deltas extracted from git XMP history.

```bash
python recipes.py capture "golden_hour" --from HEAD~1 --to HEAD
python recipes.py apply golden_hour originals/2026/05/28/*.xmp
python recipes.py list
```

Core concept: extracts **deltas** (only what changed between two git states), not full XMP snapshots. Delta-only means applying a color grade won't overwrite existing technical corrections. Recipes stack — you can apply multiple on top of each other.

Recipes saved as named JSON in `framelog/recipes/`:
```json
{
  "name": "golden_hour",
  "created": "2026-05-28",
  "settings": {
    "crs:ColorTempKelvin": "6200",
    "crs:Exposure2012": "+0.80",
    "crs:Shadows": "+35"
  }
}
```

---

### `scripts/on_sd_mount.sh`
Bash script triggered on SD card mount.

Steps:
1. Check SD card is mounted at expected path
2. Check backup drive is mounted — if not, send Mac notification and exit
3. Run `python3 ingest.py`
4. Run `rclone sync ~/Photos/originals/ /Volumes/BackupDrive/Photos/originals/`
5. Send success notification when done

Mac notification:
```bash
osascript -e 'display notification "message" with title "Framelog"'
```

---

### `launchd/com.framelog.plist`
Auto-triggers `on_sd_mount.sh` when any volume mounts.

Key keys:
- `WatchPaths`: `/Volumes`
- `ProgramArguments`: path to `on_sd_mount.sh`

Load with: `launchctl load ~/Library/LaunchAgents/com.framelog.plist`

---

## Lightroom Classic Integration

- Add `~/Photos/originals/` as source folder in Library panel
- Right-click → **Synchronize Folder** after each ingest run
- **Enable** Automatically write changes into XMP (Catalog Settings → Metadata)
- **Disable** Lightroom's own import/move behaviour
- Export finished edits to `~/Photos/processed/YYYY/MM/`
- Hit **Cmd+S** at end of every edit session to force XMP flush before watchdog commits

---

## Backup Strategy (no cloud)

| Copy | Where | Method |
|---|---|---|
| Primary | Main machine | — |
| Second local | External drive (always plugged in) | rclone on SD card insert |
| Offsite | Second drive, rotated to another location | Manual rotation |

XMP edit history → GitHub (free, tiny files)
`.lrcat` catalog → Lightroom backup scheduler → Dropbox free tier

---

## Git Repo Setup

```bash
cd ~/Photos/originals
git init
git remote add origin git@github.com:you/framelog-xmp.git
```

`.gitignore` — track only XMP files:
```gitignore
*.raf
*.RAF
*.heic
*.HEIC
*.jpg
*.jpeg
*.JPG
*.mp4
*.mov
!*.xmp
*/Lightroom\ Previews.lrdata/
*.lrprev
```

---

## Key Design Decisions

- **XMP is source of truth** — not the Lightroom catalog. Edit history survives app changes.
- **Git tracks state not operations** — each commit is a snapshot of final XMP values, not individual slider moves. Use Lightroom Snapshots at key stages for finer granularity.
- **Recipes are delta-based** — not full presets. Applying a recipe only changes what that recipe specifically touched.
- **Watchdog is battery-aware** — commits always, pushes only on AC power.
- **ingest.py is idempotent** — running it twice on the same inbox produces the same result.
