.PHONY: build build-go build-swift bundle dmg release sha test clean

VERSION      := $(shell cat VERSION)
XCODE_PROJECT := menubar/menubar/menubar.xcodeproj
SCHEME       := menubar
APP_BUILD_DIR := build/app
APP          := $(APP_BUILD_DIR)/Framelog.app
DMG          := build/Framelog-$(VERSION).dmg

build: build-go build-swift

build-go:
	cd core && go build \
		-ldflags "-X main.Version=$(VERSION)" \
		-o framelogd \
		./cmd/framelogd

build-swift:
	xcodebuild \
		-project $(XCODE_PROJECT) \
		-scheme $(SCHEME) \
		-configuration Release \
		MARKETING_VERSION=$(VERSION) \
		CURRENT_PROJECT_VERSION=$(VERSION) \
		CONFIGURATION_BUILD_DIR=$(PWD)/$(APP_BUILD_DIR) \
		build \
		| grep -E "error:|warning:|Build succeeded|BUILD FAILED" || true

# Copy framelogd into the app bundle so "Install Core" can find it.
bundle: build
	cp core/framelogd $(APP)/Contents/MacOS/framelogd

# Wrap the bundled app in a DMG for distribution.
dmg: bundle
	mkdir -p build/staging
	cp -R $(APP) build/staging/
	ln -sf /Applications build/staging/Applications
	hdiutil create \
		-volname "Framelog $(VERSION)" \
		-srcfolder build/staging \
		-ov -format UDZO \
		$(DMG)
	rm -rf build/staging
	@echo "Created $(DMG)"

# Full release artifact: Go + Swift + bundle + DMG.
release: dmg sha

# Print the sha256 of the DMG for pasting into the Homebrew cask formula.
sha:
	@shasum -a 256 $(DMG)

test:
	cd core && go test ./... -race
	xcodebuild \
		-project $(XCODE_PROJECT) \
		-scheme $(SCHEME) \
		-destination 'platform=macOS' \
		test \
		| grep -E "PASS|FAIL|error:" || true

clean:
	rm -rf core/framelogd build/
