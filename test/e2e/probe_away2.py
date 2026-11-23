#!/usr/bin/env python3
"""Why does the away probe see nothing? Watch every line both sides get."""
import socket
import sys
import time


def drain(s, secs):
    end = time.time() + secs
    out = b""
    while time.time() < end:
        try:
            out += s.recv(65536)
        except (socket.timeout, OSError):
            pass
    return out.decode("utf-8", "replace")


def observer():
    s = socket.create_connection(("salotto-e2e-ergo", 6667), timeout=10)
    s.settimeout(0.3)
    for line in ("CAP REQ :away-notify", "CAP END", "NICK obsx",
                 "USER obsx 0 * :obs"):
        s.sendall(line.encode() + b"\r\n")
    drain(s, 1.5)
    s.sendall(b"JOIN #e2e\r\n")
    drain(s, 2.0)
    return s


def bouncer(host, port, dialect, password):
    s = socket.create_connection((host, port), timeout=15)
    s.settimeout(0.3)
    if password:
        s.sendall(("PASS " + password + "\r\n").encode())
    for line in ("CAP LS 302", "NICK e2e", "USER e2e 0 * :e2e"):
        s.sendall(line.encode() + b"\r\n")
    s.sendall(b"CAP END\r\n")
    time.sleep(3.0)
    return s


which = sys.argv[1] if len(sys.argv) > 1 else "znc"
host, port = ("salotto-e2e-znc", 6667) if which == "znc" else ("salotto-e2e", 6667)
pw = "e2e-client-password" if which == "znc" else None

obs = observer()
print("observer joined; poking", which)
b = bouncer(host, port, which, pw)
before = drain(obs, 2.0)
print("--- observer before close:")
for l in before.splitlines():
    if "JOIN" in l or "AWAY" in l or " 366 " in l:
        print("  ", l.strip())
b.close()
print("bouncer closed; waiting")
after = drain(obs, 15.0)
print("--- observer after close:")
for l in after.splitlines():
    if "AWAY" in l or " QUIT " in l or " PART " in l or " NICK " in l or " JOIN" in l:
        print("  ", l.strip())
print("AWAY seen:", any(" AWAY" in l for l in after.splitlines()))
obs.close()
