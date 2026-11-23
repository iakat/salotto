#!/usr/bin/env python3
"""Probe whether away-notify works at all on this ergo, before blaming a bouncer.

Two plain clients, one goes away, does the other see it?
"""
import socket
import sys
import time


def client(nick, chan):
    s = socket.create_connection(("salotto-e2e-ergo", 6667), timeout=10)
    s.settimeout(0.3)
    for line in ("CAP REQ :away-notify", "CAP END", "NICK " + nick,
                 "USER %s 0 * :%s probe" % (nick, nick)):
        s.sendall(line.encode() + b"\r\n")
    time.sleep(1.5)
    s.sendall(("JOIN " + chan + "\r\n").encode())
    time.sleep(1.5)
    return s


def drain(s, secs):
    end = time.time() + secs
    out = b""
    while time.time() < end:
        try:
            out += s.recv(65536)
        except (socket.timeout, OSError):
            pass
    return out.decode("utf-8", "replace")


a = client("prawa", "#e2e")
b = client("prawb", "#e2e")
drain(a, 1.0)
drain(b, 1.0)
a.sendall(b"AWAY :probe reason\r\n")
seen = drain(b, 3.0)
print("RAW AWAY seen by peer:", "AWAY" in seen)
for line in seen.splitlines():
    if "AWAY" in line:
        print("  ", line.strip())
a.sendall(b"AWAY\r\n")
back = drain(b, 2.0)
print("unaway seen:", "AWAY" in back)
a.close()
b.close()
sys.exit(0 if "AWAY" in seen else 1)
