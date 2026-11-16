# Release Runbook

How to cut a new Framelog release. GitHub Actions does the build, DMG, and
release creation automatically — you only need to bump the version, tag, and
push.

---

## Prerequisites (one-time setup)

### Homebrew tap repo

Create a repo named **`homebrew-framelog`** on GitHub, then set it up:

```bash
git clone https://github.com/thevedantmodi/homebrew-framelog
cd homebrew-framelog
mkdir -p Casks
cp /path/to/framelog/homebrew/framelog.rb Casks/framelog.rb
git add . && git commit -m "add framelog cask" && git push
```

After the first real release, verify it works:
```bash
brew tap thevedantmodi/framelog
brew install --cask framelog
# if macOS blocks launch: xattr -d com.apple.quarantine /Applications/Framelog.app
```

---

## Cutting a release

### 1 — Bump the version

Edit `VERSION` in the repo root:

```bash
echo "0.2.0" > VERSION   # replace with the new version number
```

Version guidelines:
- **Patch** (`0.1.x`) — bug fix, no new user-visible behaviour
- **Minor** (`0.x.0`) — new feature, backwards-compatible
- **Major** (`x.0.0`) — breaking protocol or schema change

### 2 — Run tests

```bash
make test
```

All Go tests must pass under the race detector. Fix any failures before tagging.

### 3 — Commit, tag, push

```bash
git add VERSION
git commit -m "Bump version to 0.2.0"
git tag v0.2.0
git push origin main
git push origin v0.2.0
```

The tag push triggers the release CI workflow
(`.github/workflows/release.yml`). It:

1. Verifies the tag matches `VERSION` (fails fast if they diverge)
2. Builds `framelogd` with `-ldflags "-X main.Version=<VERSION>"`
3. Builds `Framelog.app` with `MARKETING_VERSION=<VERSION>` (unsigned)
4. Bundles `framelogd` into `Framelog.app/Contents/MacOS/`
5. Creates `Framelog-<VERSION>.dmg`
6. Opens a GitHub Release and uploads the DMG. The notes are the install
   instructions followed by GitHub's generated "What's Changed" list of PRs
   merged since the previous release (`--generate-notes`), so give PRs
   titles that read well as release notes. Edit the release afterwards to
   add a hand-written summary if you want one.
7. Updates the Homebrew tap: rewrites `version` and `sha256` in
   `Casks/framelog.rb` in `homebrew-framelog` and pushes (needs the
   `TAP_TOKEN` secret)

Monitor progress at `github.com/thevedantmodi/framelog/actions`.

### 4 — Homebrew cask (automatic)

Step 7 above updates the tap for you — check that `homebrew-framelog` got a
`framelog <VERSION>` commit. If the step failed (for example an expired
`TAP_TOKEN`), set `version` and `sha256` in the tap's `Casks/framelog.rb` by
hand; the DMG's sha256 is shown next to the asset on the release page, or run
`shasum -a 256 Framelog-<VERSION>.dmg`.

The workflow only touches those two lines. Any other cask change (a new
dependency, a new `zap` path) must be made in both the tap and
`homebrew/framelog.rb` in this repo, which is the template kept in sync with it.

### 5 — Verify the install

```bash
brew update
brew upgrade --cask framelog
# or, on a clean machine:
brew install --cask framelog
# if macOS blocks launch: xattr -d com.apple.quarantine /Applications/Framelog.app
```

Open `Framelog.app`, click **Install Core…**, confirm the daemon starts and
the menu bar shows status.

---

## Getting the sha256 manually

If you need the sha256 without a full release (e.g. to test a local DMG):

```bash
make sha
# or:
shasum -a 256 build/Framelog-<VERSION>.dmg
```

---

## Building locally without CI

To produce a DMG on your own machine:

```bash
make release   # build Go + Swift + bundle + DMG
make sha       # print sha256
```

The DMG lands at `build/Framelog-<VERSION>.dmg`. Note that without a
Developer ID the app is unsigned; right-click → Open on first launch, or:
```bash
xattr -d com.apple.quarantine /Applications/Framelog.app
```

---

## Rollback

If a release is broken:

1. Delete the GitHub release and tag:
   ```bash
   gh release delete v0.2.0 --yes
   git push --delete origin v0.2.0
   git tag -d v0.2.0
   ```
2. Revert `VERSION` and push a new tag for the previous good version.
3. In `homebrew-framelog`, revert `Casks/framelog.rb` to the previous
   version + sha256 and push.

Users who already upgraded will need to download the previous DMG from
GitHub Releases and reinstall manually.
