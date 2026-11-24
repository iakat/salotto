"""Diagnostic: run salotto-tui on a real pty against a stub bouncer and print the grid.

The model's View() is asserted to be exactly as tall as the terminal by a unit test, so a
frame that arrives cut off in the browser is either the child not knowing its size or
something writing more rows than the view. This runs the real binary on a real pty, which is
the only place both of those are real.
"""
import fcntl
import os
import pty
import select
import socket
import struct
import sys
import termios
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from vt import Term

WELCOME = [
    b":stub 001 diag :Welcome",
    b":stub 002 diag :Your host is stub",
    b":stub 375 diag :- message of the day",
    b":stub 372 diag :- hello",
    b":stub 376 diag :End of MOTD",
]


def stub_bouncer():
    """A listener that accepts one client and answers registration."""
    ln = socket.socket()
    ln.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    ln.bind(("127.0.0.1", 0))
    ln.listen(4)

    def serve():
        conn, _ = ln.accept()
        buf = b""
        while True:
            try:
                data = conn.recv(4096)
            except OSError:
                return
            if not data:
                return
            buf += data
            while b"\r\n" in buf:
                line, _, buf = buf.partition(b"\r\n")
                verb = line.split(b" ")[1] if len(line.split(b" ")) > 1 else b""
                if verb.upper() in (b"NICK", b"USER"):
                    for w in WELCOME:
                        conn.sendall(w + b"\r\n")
                    WELCOME.clear()
                if verb.upper() == b"JOIN":
                    chan = line.split(b" ")[2].split(b":")[-1]
                    conn.sendall(b":" + b"diag!d@h JOIN " + chan + b"\r\n")
                    conn.sendall(b":stub 332 diag " + chan + b" :a stub topic\r\n")

    threading.Thread(target=serve, daemon=True).start()
    return ln.getsockname()[1]


def run(binary, rows, cols, port, secs=5.0):
    env = dict(os.environ)
    env["TERM"] = "xterm-256color"
    pid, fd = pty.fork()
    if pid == 0:
        os.execve(binary, [binary, "-bouncer", "127.0.0.1:%d" % port, "-nick", "diag"], env)
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))
    buf = b""
    end = time.time() + secs
    while time.time() < end:
        r, _, _ = select.select([fd], [], [], 0.2)
        if r:
            try:
                buf += os.read(fd, 65536)
            except OSError:
                break
    try:
        os.kill(pid, 9)
    except OSError:
        pass
    t = Term(cols, rows)
    t.feed(buf)
    return t.screen()


if __name__ == "__main__":
    binary = sys.argv[1]
    rows = int(sys.argv[2]) if len(sys.argv) > 2 else 34
    cols = int(sys.argv[3]) if len(sys.argv) > 3 else 120
    port = stub_bouncer()
    scr = run(binary, rows, cols, port)
    lines = scr.split("\n")
    print("grid %dx%d, %d lines" % (cols, rows, len(lines)))
    for i, l in enumerate(lines):
        print("%2d|%s" % (i, l[:110].rstrip()))
