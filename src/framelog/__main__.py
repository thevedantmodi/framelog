import socket
import sys


def _acquire_lock() -> socket.socket:
    """Bind a Unix socket as a single-instance lock. Exits if another instance holds it."""
    sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    try:
        sock.bind("/tmp/framelog.lock")
    except OSError:
        sys.exit(0)
    return sock  # keep reference alive for process lifetime


def main():
    from framelog.config import setup_logging
    from framelog.menubar import FramelogApp
    _lock = _acquire_lock()  # noqa: F841
    setup_logging()
    FramelogApp().run()


if __name__ == "__main__":
    main()
