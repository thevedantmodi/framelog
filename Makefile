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
