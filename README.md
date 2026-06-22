# framelog (Go core + Swift menu bar)

Rewrite of the Python `framelog` pipeline. See `docs/ROADMAP.md` for the full ticket
backlog and `docs/PROTOCOL.md` for the frozen core↔frontend contract.

## Status

Phase 0 in progress:

- [x] FL-001 — repo restructured into `/core` (Go module) + `/menubar` (Swift, not yet
      created — see below) + `/docs`
- [x] FL-002 — single `VERSION` file
- [ ] FL-003 — `docs/PROTOCOL.md` started, has open questions still to resolve
- [ ] FL-004 — WAL mode noted in PROTOCOL.md, not yet implemented in `core/db`
- [ ] FL-005 — CI not set up yet

Phase 1, started:

- [x] FL-101 — `core/config`
- [x] FL-102 — `core/hasher` (with a passing test)
- [ ] FL-103 — `core/db`
- [ ] FL-104 — `core/exif`
- [ ] FL-105 — `core/xmp`
- [ ] FL-106 — `core/gitops`

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
