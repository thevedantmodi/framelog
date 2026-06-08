# Framelog

Photo ingestion, versioning, and sync pipeline for Mac. Fuji RAF + iPhone HEIC/JPG, Lightroom Classic as editor.

## How it works

1. Drop photos into `~/Photos/inbox/` (or insert SD card)
2. Ingest runs automatically — copies to `originals/YYYY/MM/DD/`, writes XMP sidecar, records in catalog
3. Lightroom syncs the originals folder and picks up new photos
4. Edit in Lightroom — XMP changes are committed to git by the watcher
5. Export finished edits to `~/Photos/processed/`

## Lightroom Setup

1. **Add originals as source** — File → Add Folder to Catalog → `~/Photos/originals/`
2. **Sync folder** — Right-click `originals/` in Library panel → Synchronize Folder (do this after each ingest)
3. **Enable auto XMP** — Lightroom → Catalog Settings → Metadata → ✅ Automatically write changes into XMP
4. **Disable Lightroom import/move** — never let Lightroom rename or move files
5. **Export path** — set export destination to `~/Photos/processed/YYYY/MM/`
6. **End of session** — hit Cmd+S to flush XMP before closing, so watcher commits everything

## Deployment

```bash
# Load inbox watcher (runs ingest on file drop)
cp launchd/com.framelog.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.framelog.plist

# Load XMP watcher (commits Lightroom edits to git)
cp launchd/com.framelog.watcher.plist ~/Library/LaunchAgents/
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.framelog.watcher.plist
```

## TODOs

- [ ] **Test SD card trigger** — load `com.framelog.sdcard.plist` and verify `on_sd_mount.sh` fires on card insert, runs ingest + rclone backup to external drive
