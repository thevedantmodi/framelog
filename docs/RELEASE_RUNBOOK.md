# Release Runbook

Step-by-step instructions for cutting a new Framelog release and publishing it to the
Homebrew tap.

---

## Prerequisites (one-time setup)

1. **Create the Homebrew tap repo** on GitHub named exactly `homebrew-framelog`:
   - Go to github.com → New repository → name: `homebrew-framelog`
   - Clone it: `git clone https://github.com/thevedantmodi/homebrew-framelog`
   - Create the directory: `mkdir -p homebrew-framelog/Casks`
   - Copy the cask template: `cp framelog/homebrew/framelog.rb homebrew-framelog/Casks/framelog.rb`
   - Commit and push: `cd homebrew-framelog && git add . && git commit -m "add framelog cask" && git push`

2. **Verify Homebrew tap works** (after at least one real release):
   ```bash
   brew tap thevedantmodi/framelog
   brew install --cask --no-quarantine framelog
   ```

---

## Cutting a release

### Step 1 — Bump the version

```bash
echo "0.2.0" > VERSION   # replace with the new version
```

Version guidelines:
- **Patch** (`0.1.x`) — bug fix, no new user-visible behaviour
- **Minor** (`0.x.0`) — new feature, no breaking changes
- **Major** (`x.0.0`) — breaking protocol/schema change, or major overhaul

### Step 2 — Run tests

```bash
make test
```

All Go tests must pass under the race detector. Fix any failures before proceeding.

### Step 3 — Build the release DMG

```bash
make release
```

This:
1. Builds `core/framelogd` with version stamp (`-ldflags "-X main.Version=<VERSION>"`)
2. Builds `Framelog.app` with matching `MARKETING_VERSION`
3. Copies `framelogd` into `Framelog.app/Contents/MacOS/`
4. Creates `build/Framelog-<VERSION>.dmg`
5. Prints the sha256 of the DMG

**Save the sha256 output** — you need it in Step 5.

### Step 4 — Create a GitHub release

1. Commit and push any pending changes:
   ```bash
   git add -A
   git commit -m "release: v<VERSION>"
   git push
   ```

2. Tag the release:
   ```bash
   git tag v<VERSION>
   git push origin v<VERSION>
   ```

3. On GitHub → Releases → Draft a new release:
   - Tag: `v<VERSION>`
   - Title: `v<VERSION>`
   - Attach: `build/Framelog-<VERSION>.dmg`
   - Publish the release

### Step 5 — Update the Homebrew cask

In your `homebrew-framelog` repo, edit `Casks/framelog.rb`:

```ruby
cask "framelog" do
  version "<VERSION>"          # ← update this
  sha256 "<sha256 from step 3>"  # ← update this

  url "https://github.com/thevedantmodi/framelog/releases/download/v#{version}/Framelog-#{version}.dmg"
  ...
end
```

Commit and push:
```bash
cd homebrew-framelog
git add Casks/framelog.rb
git commit -m "framelog <VERSION>"
git push
```

### Step 6 — Verify the install

```bash
brew update
brew upgrade --cask framelog
# or fresh install:
brew install --cask --no-quarantine framelog
```

Open `Framelog.app`, click **Install Core…**, confirm the daemon starts and the menu bar
shows status.

---

## Getting the sha256 manually

If you need the sha256 outside of `make release`:

```bash
make sha
# or:
shasum -a 256 build/Framelog-<VERSION>.dmg
```

---

## Rollback

If a release is broken:
1. Delete the GitHub release and tag
2. Revert `VERSION` to the previous value
3. Revert `homebrew-framelog/Casks/framelog.rb` to the previous version + sha256
4. Push the revert

Users who already upgraded will need to run `brew install --cask --no-quarantine framelog@<previous>` or manually download the previous DMG from GitHub releases.
