#!/usr/bin/env bash
set -euo pipefail

BACKUP_PATH="${BACKUP_PATH:-/Volumes/BackupDrive}"
FRAMELOG_DIR="$(cd "$(dirname "$0")/.." && pwd)"

notify() {
    osascript -e "display notification \"$1\" with title \"Framelog\""
}

log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') $*"
}

# Find SD card: removable volume with DCIM folder
SD_PATH=""
for vol in /Volumes/*/; do
    if diskutil info "$vol" 2>/dev/null | grep -q "Removable Media:.*Removable"; then
        if [ -d "${vol}DCIM" ]; then
            SD_PATH="$vol"
            break
        fi
    fi
done

if [ -z "$SD_PATH" ]; then
    log "No SD card found"
    exit 0
fi

log "SD card: $SD_PATH"

# Check backup drive
# if [ ! -d "$BACKUP_PATH" ]; then
#     notify "Backup drive not mounted — connect it and re-insert SD card"
#     log "Backup drive not found at $BACKUP_PATH"
#     exit 1
# fi

# Sync SD → backup in background while ingest runs
# rclone sync "${SD_PATH}DCIM/" "${BACKUP_PATH}/Photos/raw_sd/" --progress &
# RCLONE_PID=$!

# Copy SD card photos to inbox
log "Copying from ${SD_PATH}DCIM/ to inbox"
cp -rn "${SD_PATH}DCIM/"* ~/Photos/inbox/ 2>/dev/null || true

# Signal app to run ingest
touch ~/Photos/.ingest_trigger
log "Ingest trigger set"
