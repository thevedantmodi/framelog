# framelog (Go core + Swift menu bar)

Rewrite of the Python `framelog` pipeline. See `docs/ROADMAP.md` for the full ticket
backlog and `docs/PROTOCOL.md` for the frozen core↔frontend contract.

## Status

Phase 0 in progress:

- [x] FL-001 — repo restructured into `/core` (Go module) + `/menubar` (Swift, not yet
      created — see below) + `/docs`
- [x] FL-002 — single `VERSION` file
- [ ] FL-003 — `docs/PROTOCOL.md` started, has open questions still to resolve
- [x] FL-004 — WAL mode + busy_timeout + read-only DSN enforced in `core/db`
- [x] FL-005 — CI: `.github/workflows/ci.yml` (ubuntu-latest, go vet + go test -race -cover)

Phase 1, complete:

- [x] FL-101 — `core/config`
- [x] FL-102 — `core/hasher` (with a passing test)
- [x] FL-103 — `core/db` (InitDB, InsertPhoto, HashExists, UpdateStatus, PhotoCount, LastImport)
- [x] FL-104 — `core/exif` (FindExiftool, ReadExif with injectable binary path)
- [x] FL-105 — `core/xmp` (WriteXMP, xpacket wrapper, dc:subject keyword bag, no crs: namespace)
- [x] FL-106 — `core/gitops` (FindGit, Commit, FindPmset, IsOnACPower, Push)

Phase 2, complete:

- [x] FL-206 — `core/logging` (done out of order: ingest needed it before FL-201 could be written)
- [x] FL-201 — `core/ingest` (Pipeline, ImportFile copy-before-delete, RunIngest with git commit/push; concurrency guard patched in FL-203; backup call added in FL-207)
- [x] FL-202 — `core/outgest` (Pipeline, OrganizeFile, RunOutgest, UpdateStatusByHashPrefix in db)
- [x] FL-203 — `core/sdcard` (FindDiskutil, IsRemovableMedia, HasDCIM, FindSDCard, CopyDCIM, Watcher/Run/Stop; also patched FL-201's RunIngest with ErrIngestAlreadyRunning + TryAcquire/Release guard)
- [x] FL-204 — `core/xmpwatcher` (FindPgrep, IsLightroomRunning, Watcher/Run/Stop; debounce+commit+push-gated-on-AC-and-LR-closed; also refactored hash8 regex out of outgest into db.ExtractHashPrefix)
- [x] FL-205 — `core/outgestwatcher` (OutgestRunner interface, Watcher/Run/Stop; single-dir non-recursive watch; directory-create events explicitly ignored; debounce collapses bursts; also patched FL-202's RunOutgest with ErrOutgestAlreadyRunning + TryAcquire/Release guard)
- [x] FL-207 — `core/backup` (FindRclone, Sync with rclone-copy-not-sync; added RclonePath/BackupPath to ingest.Pipeline; backup gated only on counts.Imported>0 and BackupPath!="" — deliberately decoupled from git push; corrects FL-201 TODO which incorrectly implied backup should follow push)

Phase 2 hardening, complete:

- [x] FL-005 — CI in place (see Phase 0 above); ubuntu-latest runner chosen to enforce
      injectable-binary-path pattern — no real exiftool/diskutil/pmset/pgrep/rclone on PATH
- [x] Concurrency integration test — `core/integration_test.go::TestConcurrent` starts all
      three watchers (sdcard, xmpwatcher, outgestwatcher) against a shared catalog.db and
      originals/ git repo, fires all three triggers simultaneously, and asserts consistent
      DB rows, clean git commit messages, and parseable log lines under the race detector
      (`go test ./... -race -run TestConcurrent -count=5`)
- [x] Invariants regression suite — one `*_invariants_test.go` per package, each test named
      after the guarantee it encodes: `TestInvariant_CopyBeforeDelete` (FL-201),
      `TestInvariant_NonRecursiveListing` (FL-202), `TestInvariant_ConcurrentRunRejected`
      (FL-203/FL-205), `TestInvariant_GitDirExcluded` (FL-204),
      `TestInvariant_BackupIndependentOfGitPush` (FL-207)

Coverage (all tests, `go test ./... -race -cover`):

| Package           | Coverage | Notes on uncovered branches                                      |
|-------------------|----------|------------------------------------------------------------------|
| core/backup       | 93.8%    |                                                                  |
| core/db           | 79.0%    | `UpdateStatus` no-row error path; `LastImport` NULL scan path    |
| core/exif         | 87.9%    |                                                                  |
| core/gitops       | 74.5%    | `FindPmset` (macOS-only, no PATH fallback exercised); `Commit` stderr capture branch; `IsOnACPower`/`Push` stderr paths |
| core/hasher       | 87.5%    |                                                                  |
| core/ingest       | 79.6%    | `fail()` path in `ImportFile` after XMP write; `copyFile` partial-write error |
| core/logging      | 72.2%    | `New` open-failure path; `Log` file-write-failure/sync-failure paths; `Close` sync-failure path — all require injecting I/O errors |
| core/outgest      | 80.4%    |                                                                  |
| core/outgestwatcher | 86.7% |                                                                  |
| core/sdcard       | 77.9%    | `Run` fsnotify-open error; `copyFile` partial-write error; `FindSDCard` readdir error |
| core/xmp          | 96.3%    |                                                                  |
| core/xmpwatcher   | 82.9%    |                                                                  |

Packages under 80%: `core/gitops` (74.5%), `core/logging` (72.2%), `core/sdcard` (77.9%),
`core/db` (79.0%), `core/ingest` (79.6%). The uncovered paths in all five are error branches
that require I/O fault injection (write failures, fsnotify init failures, macOS-only binaries)
rather than missing test coverage for happy paths or guard conditions.

## Building the core

```bash
cd core
go build ./...
go test ./...
```

## Creating the Swift menu bar shell

Not scaffolded here — it needs to be created in Xcode on macOS (App target, not a bare
SwiftPM executable, so you get an `Info.plist` to set `LSUIElement` and proper code
signing). In Xcode: **File → New → Project → macOS → App**, save it into `menubar/` in
this repo. See the roadmap's FL-401/FL-402 for what goes in it first.
