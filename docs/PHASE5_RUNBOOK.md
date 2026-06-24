# Phase 5 Runbook — Cutover to framelogd

This is a human-executable checklist for a clean first-run of the new Go core,
decommissioning the old Python app, and confirming real hardware behavior.
Nothing in here requires a migration — the user confirmed the existing test
data in `~/Photos` is disposable.

**You cannot execute this runbook yourself (inserting an SD card, clicking
through Lightroom, confirming live log output). Follow it step by step and
note the outcome of each step before proceeding.**

---

## Prerequisites

- `exiftool` and `git` installed and on PATH (the daemon will refuse to start
  without these — they are the only hard requirements).
- `~/Photos/` contains only your real library in `all-photos/` (or is empty).
  The three working directories (`inbox/`, `originals/`, `processed/`) will be
  created fresh by `framelogd run`.
- The repo is at `~/dev/framelog/` and you have a built binary ready (step 2).

---

## Step 1 — Unload the old Python launchd jobs

The old Python app runs two launchd jobs. Unload them before starting the new
core to avoid both watchers racing for the same `/Volumes` events.

```bash
launchctl bootout gui/$(id -u)/com.framelog.sdcard 2>/dev/null || true
launchctl bootout gui/$(id -u)/com.framelog.app    2>/dev/null || true
launchctl list | grep framelog
```

- [x] **Expected output:** `com.framelog.sdcard` and `com.framelog.app` are no
  longer listed. The `|| true` suppresses errors if they were never loaded.

**If this fails:** Run `launchctl print gui/$(id -u)/com.framelog.sdcard` to
see why bootout is rejected. On macOS 13+, `bootout` replaces `unload` — do
not use `launchctl unload` with the plist path for the new-style GUI domain.

---

## Step 2 — Build and run in the foreground

```bash
cd ~/dev/framelog/core
go build -ldflags "-X main.Version=$(cat ../VERSION)" -o framelogd ./cmd/framelogd
./framelogd run
```

Watch the log output. Within ~1 second you should see lines matching this
pattern (exact paths will vary):

```
2026-06-24 HH:MM:SS [CORE] framelogd 0.1.0 starting
2026-06-24 HH:MM:SS [CORE] exiftool: /opt/homebrew/bin/exiftool
2026-06-24 HH:MM:SS [CORE] git: /usr/bin/git
2026-06-24 HH:MM:SS [CORE] pmset: /usr/bin/pmset          ← or "not found" note
2026-06-24 HH:MM:SS [CORE] pgrep: /usr/bin/pgrep          ← or "not found" note
2026-06-24 HH:MM:SS [CORE] diskutil: /usr/sbin/diskutil   ← or "SD card watcher disabled"
2026-06-24 HH:MM:SS [CORE] rclone: /opt/homebrew/bin/rclone ← or "backup disabled"
```

Then the three work directories are created, `catalog.db` is initialised, and
`originals/` is `git init`-ed. Check:

```bash
ls ~/Photos/inbox ~/Photos/originals ~/Photos/processed
git -C ~/Photos/originals rev-parse --git-dir   # should print ".git"
```

- [x] **Expected:** All three directories exist; `git -C originals rev-parse`
  exits 0.

**If exiftool or git is not found:** The daemon logs the error and exits 1.
Install the missing binary (`brew install exiftool` / `brew install git`) and
re-run.

**If pmset/pgrep/diskutil/rclone shows "not found":** This is expected
degraded mode — note exactly which ones are missing and confirm the log
matches the degradation described (no push gating, Lightroom check skipped, SD
card watcher disabled, backup disabled). Missing optional binaries are not bugs.

---

## Step 3 — Smoke-test the v2 IPC socket from a second terminal

With `framelogd run` still running in the first terminal:

```bash
echo '{"command":"status"}' | nc -U ~/Library/"Application Support"/Framelog/framelog.sock
```

- [x] **Expected:** A single JSON line back within 1–2 seconds:
  ```json
  {"protocol_version":1,"ok":true,"ingest_running":false,"outgest_running":false,"photo_count":0,"last_import":"","backup_drive_mounted":false}
  ```

This is the first time anything has dialed this socket against a process with
real binaries on both ends (all previous tests used fake scripts).

**If nc returns immediately with no output:** The socket file is missing. Check
that `ls ~/Library/Application\ Support/Framelog/framelog.sock` exists —
if not, look at the startup log for an IPC error.

**If nc hangs:** The server's 5-second `ReadDeadline` will close the connection.
Check the daemon log for a JSON parse error — `nc` may have written a trailing
newline that confused the parser. Use `printf '{"command":"status"}\n'` instead.

---

## Step 4 — Insert a real SD card

Insert a card with a real DCIM folder. Watch the first terminal.

Expected sequence in the log:
1. `[CORE] SD card detected: /Volumes/<card name>` — within ~2 seconds of mount
2. `[CORE] CopyDCIM: copied N files to inbox/`
3. `[INGEST] Done: N imported, 0 skipped, 0 failed`
4. `[GIT] commit: import <batch-id>` (or push line if on AC power)

Verify on disk:
```bash
ls ~/Photos/originals/          # should show YYYY/ subdirs
git -C ~/Photos/originals log --oneline   # should show one commit
```

- [x] **Expected:** Files appear in `originals/YYYY/MM/DD/` with `.xmp`
  sidecars; one git commit exists.

**If nothing happens after 5+ seconds:** Run
`diskutil info /Volumes/<card name>` and check the "Removable Media:" line.
`IsRemovableMedia` looks for the substring "Removable" in the value portion of
that line (not the key), excluding "Not Removable". If your card reports
something unexpected (e.g. "Fixed"), that is a real data point — the regex has
only been tested against a fake script before now, and a real card may need a
regex update in `core/sdcard/sdcard.go:IsRemovableMedia`.

**If push is skipped:** If on battery power, the log should say
`[GIT] skipping push: not on AC power`. If pmset was not found, the log says
the check is skipped and push will always be attempted — confirm which case
applies.

---

## Step 5 — Re-insert the same card (dedup test)

Eject and re-insert the same card without deleting any files from it.

- [x] **Expected:** The log shows `[INGEST] Done: 0 imported, N skipped, 0
  failed` — every file recognised as a duplicate via SHA-256 hash, nothing
  re-imported.

This is the first real-world dedup test against a real card (all prior dedup
tests used synthetic hashes in temp directories).

**If files are re-imported:** The hash of a file changed between the two
inserts, which should not happen for static image files. Check if the card's
filesystem is modifying file metadata on unmount/remount.

---

## Step 6 — Outgest trigger file test

Put at least one file into `~/Photos/processed/` (copy any `.jpg` there), then:

```bash
touch ~/Photos/.outgest_trigger
```

- [x] **Expected:** Within ~2 seconds the log shows:
  ```
  [OUTGEST] Done: N moved, 0 skipped, 0 failed
  ```
  and the file has been moved from `processed/` into `processed/YYYY/MM/`.

This is the first real exercise of the `.outgest_trigger` path added in Phase 3
(FL-301 + FL-404 contract extension). All prior tests used a fake runner.

**If nothing happens within 5 seconds:** Check that `~/Photos/.outgest_trigger`
no longer exists — the triggerwatcher removes the file before calling the
runner, so if the file is still there, the watcher hasn't ticked yet (poll
interval is 2 seconds). If the file is gone but no outgest log line appeared,
look for an error log line from the outgest runner.

---

## Step 7 — Backup drive (optional, skip if no BACKUP_PATH)

If `FRAMELOG_BACKUP_PATH` is set and the backup drive is mounted:

1. Trigger one more ingest (touch `.ingest_trigger` or insert a card with new
   photos).
2. Check the log for:
   ```
   [BACKUP] rclone copy originals/ → <BACKUP_PATH>/originals/: OK
   ```
3. Verify: `ls $FRAMELOG_BACKUP_PATH/originals/` shows the new file.

- [x] **If rclone was not found:** Skip this step (backup is already noted as
  disabled in the startup log).

**If rclone errors:** The log includes stderr from rclone. Common causes: the
drive is mounted but `BACKUP_PATH` points inside it incorrectly, or rclone
needs authentication for a remote target.

---

## Step 8 — Lightroom round-trip (optional, skip if Lightroom not installed)

1. Open a photo in Lightroom from `originals/`.
2. Make a small edit and save.
3. Watch the log for the debounce window to fire (`[XMP] committing N changed
   files`). While Lightroom is still open, confirm the log says push was
   withheld (if pgrep is available and Lightroom is detected as running).
4. Close Lightroom.
5. Make one more small edit, save.
6. After the debounce window, confirm `[GIT] push` appears (assuming AC power
   and pgrep available).

- [x] **Expected:** Two commits, one with push withheld, one pushed.

**If no XMP log appears:** The debounce is 10 seconds (`config.DebounceSeconds`).
Wait at least 15 seconds after the last save. If still nothing, check that
`originals/` is being watched — the XMP watcher recurses subdirectories on
startup, so a photo added after the first `git init` should already be watched.

---

## Step 9 — Graceful shutdown

Press `Ctrl-C` in the terminal running `framelogd run`.

```bash
ls ~/Library/"Application Support"/Framelog/framelog.sock 2>&1
```

- [x] **Expected:** Log shows `[CORE] received signal, shutting down`. The
  socket file is **gone** — `Stop()` removes it.

**If the socket still exists:** This is a real bug. `ipc.Server.Stop()` calls
`os.Remove(socketPath)` — if the file persists, something held the server's
`ln` reference beyond shutdown. Report this before running `framelogd install`.

---

## Step 10 — Install as a launchd agent

```bash
./framelogd install
launchctl list | grep com.framelog.core
```

- [x] **Expected:** `framelogd install` prints `framelogd: installed and
  bootstrapped`. `launchctl list` shows `com.framelog.core` with a numeric PID
  (not `-`), meaning it started successfully.

What `install` actually does (from `core/launchd/launchd.go`):
1. Creates `~/Library/LaunchAgents/com.framelog.core.plist` (and the directory
   if needed).
2. Creates `~/Library/Logs/Framelog/` (so launchd can open `crash.log` on first
   write).
3. Runs `launchctl bootout gui/<uid>/com.framelog.core` (silently, for
   idempotency).
4. Runs `launchctl bootstrap gui/<uid> <plist path>`.

The plist sets `RunAtLoad=true` and `KeepAlive=true`, and points
`StandardOutPath`/`StandardErrorPath` at `~/Library/Logs/Framelog/crash.log`
(not at `~/Photos/framelog.log`, which the logger writes directly — see
PROTOCOL.md §3 for the rationale).

Now repeat step 4 (SD card insert) against the launchd-managed process:
- [x] Check `~/Photos/framelog.log` for the same ingest sequence.
- [x] Check `~/Library/Logs/Framelog/crash.log` is **empty**. Anything in it
  is a signal worth reading — it captures panics and pre-logger failures, not
  routine output. Non-empty crash log = real problem.

**If `launchctl list` shows PID `-` with a non-zero exit code:** Run
`launchctl print gui/$(id -u)/com.framelog.core` to see the last exit status
and reason. Most common causes: binary path in the plist is wrong (check
`os.Executable()` returned the right path), or one of the required binaries
(exiftool, git) is not on the PATH that launchd uses (launchd's PATH is
shorter than your shell's — may need absolute paths added via `EnvironmentVariables`
in the plist in a future ticket).

---

## Step 11 — Decommission the old Python app

```bash
# Remove the old plist files.
rm ~/Library/LaunchAgents/com.framelog.sdcard.plist
rm ~/Library/LaunchAgents/com.framelog.app.plist

# Remove the old script and state directory.
rm ~/.framelog/on_sd_mount.sh
# Leave ~/.framelog/catalog.db and framelog.log in place for reference,
# or remove the whole directory if you no longer need them:
# rm -rf ~/.framelog/

# Kill the Python menubar.py process if it was still running
# (it likely auto-exits when its LaunchAgent is gone).
pkill -f "menubar.py" 2>/dev/null || true
```

Verify:
```bash
launchctl list | grep framelog   # should show only com.framelog.core
ls ~/Library/LaunchAgents/ | grep framelog  # should show only com.framelog.core.plist
```

- [x] **Expected:** Only `com.framelog.core` remains. Python app is gone.

**If the Python plist files are still loaded after removal:** Removing a plist
file does not automatically unload the job. Run
`launchctl bootout gui/$(id -u)/com.framelog.sdcard` first, then remove the
file.

---

## Post-cutover checklist

- [ ] `launchctl list | grep framelog` shows only `com.framelog.core` with a
  live PID.
- [ ] `~/Library/Logs/Framelog/crash.log` is empty (or doesn't exist yet).
- [ ] `~/Photos/framelog.log` contains structured `[INGEST]`/`[OUTGEST]`/etc.
  lines from the new core — no bare Python `print()` output.
- [ ] The Swift menu bar app shows status from the new `catalog.db`.
- [ ] "Run Ingest Now" and "Run Outgest Now" buttons in the menu bar create
  trigger files that the new core picks up within ~2 seconds.
- [ ] (When FL-404 is migrated to the socket) Test those buttons again; confirm
  `~/Library/Application Support/Framelog/framelog.sock` responds.
