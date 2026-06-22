VERSION := 1.0.0
APP     := dist/Framelog.app
DMG     := dist/Framelog-$(VERSION).dmg

.PHONY: build sign dmg release clean

build:
	rm -rf build/ dist/
	PYTHONPATH=src .venv-build/bin/python setup.py py2app

sign: $(APP)
	codesign --deep --force --sign "-" $(APP)

dmg: sign
	rm -f $(DMG)
	hdiutil create -volname "Framelog" -srcfolder $(APP) -ov -format UDZO $(DMG)
	@echo "Built $(DMG)"

release: build dmg

clean:
	rm -rf build/ dist/

reset:
	-launchctl unload ~/Library/LaunchAgents/com.framelog.app.plist 2>/dev/null
	-launchctl unload ~/Library/LaunchAgents/com.framelog.sdcard.plist 2>/dev/null
	-pkill -f Framelog 2>/dev/null; true
	rm -f ~/Library/LaunchAgents/com.framelog.app.plist
	rm -f ~/Library/LaunchAgents/com.framelog.sdcard.plist
	rm -rf ~/.framelog/
	rm -f ~/Photos/inbox/*
	rm -f ~/.framelog_setup_done ~/.framelog_sd_paused ~/.framelog_on_sd_mount.sh
	rm -f ~/Photos/.ingest_trigger ~/Photos/catalog.db ~/Photos/framelog.log
	rm -f /tmp/framelog.lock
	@echo "Reset complete. Run: make release && open dist/Framelog.app"
