import sys
import py2app.build_app as _build_app
from py2app.build_app import py2app as _py2app
from setuptools import setup

# py2app 0.28+ rejects install_requires injected from pyproject.toml
_orig_check = _build_app.py2app.finalize_options

def _patched_finalize(self):
    self.distribution.install_requires = None
    _orig_check(self)

_build_app.py2app.finalize_options = _patched_finalize

APP = ['src/framelog/__main__.py']
OPTIONS = {
    'argv_emulation': False,
    'plist': {
        'CFBundleName': 'Framelog',
        'CFBundleIdentifier': 'com.framelog.app',
        'CFBundleVersion': '1.0.0',
        'LSUIElement': True,
    },
    'packages': ['rumps', 'psutil', 'watchdog', 'framelog'],
    'iconfile': 'assets/framelog.icns',
    'resources': ['src/framelog/on_sd_mount.sh'],
}

setup(
    name='Framelog',
    app=APP,
    options={'py2app': OPTIONS},
)
