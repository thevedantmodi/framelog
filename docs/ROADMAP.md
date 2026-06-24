# Framelog Rewrite Roadmap — Go core + Swift menu bar

**Architecture:** one headless Go binary (`framelogd`) owns the entire pipeline — SD-card
watching, ingest, outgest, XMP/git, backup — and runs as a `launchd` `KeepAlive` agent.
A separate Swift app (`Framelog.app`) is a thin menu-bar shell with no pipeline logic; it
talks to the core over IPC and reads `catalog.db`/`framelog.log` directly for status.

This backlog is written so Phase 1–3 (Go) and Phase 4 (Swift skeleton) can run **in
parallel** once Phase 0 is done — the only hard dependency between them is the protocol
defined in FL-003.

---

## Phase 0 — Foundations

**Goal:** Decide the seams between Go and Swift *before* writing pipeline code, and avoid
re-creating the build-config mess found in the Python version.

- **FL-001 — Repo restructure for two-binary layout**
  `/core` (Go module), `/menubar` (Swift package/Xcode project), `/docs` shared.
  *Acceptance:* `go build ./core/...` and `xcodebuild -project menubar` both succeed from a
  clean checkout.

- **FL-002 — Single source of truth for version number**
  One `VERSION` file (or git tag) read by both the Go build (`-ldflags`) and the Swift
  build (`CFBundleVersion`/`CFBundleShortVersionString`).
  *Acceptance:* bumping one file changes the version reported by both binaries; no version
  string is hardcoded a second time anywhere.

- **FL-003 — `PROTOCOL.md`: define the core↔frontend contract**
  Trigger-file fields (v1 IPC), command/response shapes (v2 IPC), log line format,
  `catalog.db` schema as a frozen contract. Write this before FL-1xx/FL-4xx so both sides
  can build against it independently.
  *Acceptance:* a teammate could implement either side from this doc alone, without
  reading the other side's code.

- **FL-004 — SQLite access policy**
  `PRAGMA journal_mode=WAL`, `busy_timeout` set, core is the only writer, frontend opens
  read-only connections.
  *Acceptance:* documented in PROTOCOL.md; enforced in `db` package via a read-only DSN
  option.

- **FL-005 — CI skeleton**
  GitHub Actions: `go build && go test ./...` job, `xcodebuild test` job.
  *Acceptance:* CI is green on a runner that does **not** have `exiftool` installed —
  decide now that no test may depend on a real external binary being on `PATH` (this is
  the exact bug class found in `test_exif.py` in the Python version).

---

## Phase 1 — Go core: pipeline primitives

**Goal:** Port the leaf modules — no orchestration, no watchers, fully unit-testable in
isolation.

- **FL-101 — `config` package** — paths, supported extensions, constants.
- **FL-102 — `hasher` package** — SHA-256, chunked reads.
- **FL-103 — `db` package** — schema, `InitDB`/`InsertPhoto`/`HashExists`/`UpdateStatus`,
  WAL mode per FL-004.
  *Decision required:* either wire `UpdateStatus` into the pipeline this time (ingest →
  `raw`, export → `published`) or drop the status column entirely. Don't ship an unused
  lifecycle field again.
- **FL-104 — `exif` package** — exiftool subprocess wrapper.
  *Acceptance:* the resolved-binary lookup is injectable, so tests can fully fake exiftool
  without touching the real `PATH` or relying on it being absent/present.
- **FL-105 — `xmp` package** — sidecar writer.
  *Decision required:* are `crs:` develop-settings tags in scope for v1? If not, don't
  register the namespace.
- **FL-106 — `gitops` package** — commit/push against `originals/`.
  *Evaluate:* shell out to the `git` CLI (simplest port) vs. `go-git` (drops the external
  binary dependency). Pick one and document why in the ticket.

---

## Phase 2 — Go core: orchestration & watchers

**Goal:** Wire primitives into actual pipeline behavior; replace `on_sd_mount.sh` and the
threaded watchers from `menubar.py`.

- **FL-201 — `ingest` package** — `ImportFile`/`RunIngest`.
  *Acceptance:* GPS fields from FL-104 actually flow into the inserted record if FL-103
  decided to keep them — no more "read and discarded" data.
- **FL-202 — `outgest` package** — `OrganizeFile`/`RunOutgest`.
- **FL-203 — SD card watcher** — in-process watcher (poll `/Volumes` or use `fsnotify`)
  inside the long-running core. Eliminates the separate `WatchPaths`-triggered launchd job
  and bash script entirely — the core is already running, it can just watch.
- **FL-204 — XMP change watcher** — port `XMPHandler`'s debounce-and-commit to a goroutine
  + `time.AfterFunc`, gated on whether Lightroom is running.
- **FL-205 — Outgest watcher** — port `OutgestHandler` similarly.
- **FL-206 — Structured logging** — one logger, used everywhere, always flushed. (No
  repeat of the `print()` vs. `log()` split between `ingest.py` and `outgest.py`.)
- **FL-207 — `backup` package** — `rclone` wrapper syncing `originals/` (the deduped
  library, post-ingest) to `BACKUP_PATH`. This finally closes the gap between what the
  design doc described and what the old script actually did (which backed up the raw SD
  card pre-ingest instead). Use `rclone copy`, not `sync`, so deletions on the source side
  don't propagate to the backup.

---

## Phase 3 — IPC & launchd

**Goal:** Make the core controllable from outside and self-installing as a real agent.

- **FL-301 — v1 IPC: trigger file + poll**
  Port `.ingest_trigger` as-is — fastest path to an end-to-end working system.
- **FL-302 — v2 IPC: Unix domain socket**
  Line-delimited JSON, commands: `ingest_now`, `outgest_now`, `status`. Replace polling
  once the rest of the pipeline is stable.
- **FL-303 — Core's own launchd plist**
  Core generates and installs its own `KeepAlive` agent plist on first run (resolving the
  real executable path and home directory itself) rather than shipping a static plist with
  a hardcoded username.
- **FL-304 — Headless CLI mode**
  Core runs and logs sensibly with zero frontend attached, for debugging the pipeline
  without the menu bar in the loop at all.

---

## Phase 4 — Swift menu bar frontend

**Goal:** Thin status/control surface. No pipeline logic lives here — if you're tempted to
add any, it belongs in Phase 2 instead.

- **FL-401 — App skeleton**
  `MenuBarExtra` (or `NSStatusItem` if you need more control than SwiftUI gives you),
  `LSUIElement` set, no Dock icon. Can start as soon as FL-001 lands — doesn't need the Go
  core to exist yet.
- **FL-402 — `SMAppService` login-item registration**
  Replaces the manual `launchctl load`/`unload` dance from `firstrun.py`.
- **FL-403 — Status display**
  Read `catalog.db` read-only (photo count, last import) and tail `framelog.log` —
  Swift-side equivalents of `_photo_count()`/`_last_import()`/`_tail_log()`. Needs FL-004's
  read-only access policy and FL-103's finalized schema.
- **FL-404 — Manual controls**
  "Run Ingest Now" / "Run Outgest Now", wired to the IPC client. Start against FL-301, swap
  to FL-302 once it lands.
- **FL-405 — Native notifications**
  `UserNotifications` framework for ingest/outgest completion and backup-drive-missing
  warnings.
- **FL-406 — "Open Log File" / "Run Setup" menu items.**

---

## Phase 5 — Migration & cutover ✓ complete

**Goal:** Move off the running Python app without losing photo history or breaking
Lightroom's relationship to `originals/`.

- **FL-501 ✓ — `catalog.db` compatibility check** — cold-start test verifies schema
  creation; real-hardware ingest verified against SD card.
- **FL-502 ✓ — Side-by-side dry run** — real-hardware SD card + Lightroom XMP pipeline
  tested end-to-end. DNG XMP extraction, git commit on edit, outgest verified.
- **FL-503 ✓ — Decommission old jobs** — Python version fully replaced. No
  `com.framelog.sdcard`, no `menubar.py`, no `on_sd_mount.sh`.
- **FL-504 ✓ — Docs rewritten** — `README.md`, `PROTOCOL.md`, `ROADMAP.md`, `CLAUDE.md`
  all reflect the actual shipped architecture.

---

## Phase 6 — Polish & distribution

- **FL-601 — Codesigning + notarization** *(deferred — requires $99 Apple Developer
  account)* — app runs unsigned for personal use; Homebrew users need `--no-quarantine`.
  Revisit when distributing publicly.

- **FL-602 ✓ — Single version number end-to-end** — root `VERSION` file drives both
  `framelogd --version` (via `-ldflags "-X main.Version=$(cat VERSION)"`) and
  `CFBundleShortVersionString` (via `xcodebuild MARKETING_VERSION=$(VERSION)`). Bump
  `VERSION`, run `make release`, done.

- **FL-603 ✓ — Installer** — `framelogd` bundled inside `Framelog.app/Contents/MacOS/`.
  `make release` builds Go + Swift, copies binary into bundle, wraps in DMG.
  "Install Core…" button in the menu bar runs the bundled `framelogd install`.
  Distribution: GitHub Releases DMG + Homebrew tap (`homebrew/framelog.rb`).
  Install: `brew tap thevedantmodi/framelog && brew install --cask --no-quarantine framelog`.

- **FL-604 ✓ — Crash/restart policy** — `KeepAlive=true` in launchd plist; crash output
  goes to `~/Library/Logs/Framelog/crash.log`. Menu bar uses a POSIX socket ping to
  distinguish four states: never installed / restarting (launchd recovering) / no photos /
  normal. See `PROTOCOL.md §4`.

### Pending (FL-404 follow-up)

- **Socket migration for Swift buttons** — "Run Ingest Now" / "Run Outgest Now" currently
  touch trigger files (v1 IPC). Should send `{"command":"ingest_now"}` /
  `{"command":"outgest_now"}` over the socket. Once done, both trigger files can be
  retired from `PROTOCOL.md §2`.

---

## Definition of Done (applies to every ticket)

- [ ] No new external dependency is added without checking it's actually used (the
      Python version shipped an unused `crs:` namespace and an unused `status` lifecycle —
      don't repeat that).
- [ ] Tests don't depend on a real external binary (`exiftool`, `git`, `rclone`) being
      present unless that's specifically what the test is checking.
- [ ] Any change to the core↔frontend contract is reflected in `PROTOCOL.md` in the same
      PR, not a follow-up.
- [ ] Any change to installed architecture (new launchd job, new file location) is
      reflected in `README.md` in the same PR.

## Suggested sequencing

```
Phase 0 ──────────────────────────────────────────┐
   │                                               │
   ▼                                               ▼
Phase 1 (Go primitives) ──► Phase 2 (orchestration)   Phase 4 (Swift skeleton: FL-401/402)
   │                              │                       │
   └──────────► Phase 3 (IPC) ◄───┘                       │
                     │                                    │
                     └────────────► FL-403/404 ◄───────────┘
                                          │
                                    Phase 5 ──► Phase 6
```

Phase 4's FL-401/402 don't need the Go core at all and can start the same day as Phase 1.
Everything else funnels through the IPC contract from FL-003/FL-301/FL-302.
