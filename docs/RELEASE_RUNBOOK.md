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
brew install --cask --no-quarantine framelog
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
6. Opens a GitHub Release, uploads the DMG, and prints the sha256 in the
   release notes

Monitor progress at `github.com/thevedantmodi/framelog/actions`.

### 4 — Update the Homebrew cask

After the GitHub Release is published, copy the sha256 from the release notes
and edit `Casks/framelog.rb` in your `homebrew-framelog` repo:

```ruby
cask "framelog" do
  version "0.2.0"                    # ← new version
  sha256 "<sha256 from release notes>" # ← paste here

  url "https://github.com/thevedantmodi/framelog/releases/download/v#{version}/Framelog-#{version}.dmg"
  ...
end
```

Commit and push:
```bash
cd homebrew-framelog
git add Casks/framelog.rb
git commit -m "framelog 0.2.0"
git push
```

### 5 — Verify the install

```bash
brew update
brew upgrade --cask framelog
# or, on a clean machine:
brew install --cask --no-quarantine framelog
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
Developer ID the app is unsigned; users need `--no-quarantine` or must
right-click → Open on first launch.

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
