#!/usr/bin/env python3
"""Decisive away probe: ask ZNC's own module, and print everything the observer
sees. If *simple_away says the timer is set but no AWAY reaches ergo, the
problem is upstream of the module.
"""
import socket
import sys
import time

MODE = sys.argv[1] if len(sys.argv) > 1 else "znc"
HOST, PORT = ("salotto-e2e-znc", 6667) if MODE == "znc" else ("salotto-e2e", 6667)
PW = "e2e-client-password" if MODE == "znc" else None


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
    for line in ("CAP LS 302", "CAP REQ :away-notify", "CAP END", "NICK obsy",
                 "USER obsy 0 * :obs"):
        s.sendall(line.encode() + b"\r\n")
    drain(s, 2.0)
    s.sendall(b"JOIN #e2e\r\n")
    names = drain(s, 2.5)
    print("--- observer NAMES block:")
    for l in names.splitlines():
        if " 353 " in l or " 366 " in l or " 4" in l[:8]:
            print("   ", l.strip())
    return s


def bouncer():
    s = socket.create_connection((HOST, PORT), timeout=15)
    s.settimeout(0.3)
    if PW:
        s.sendall(("PASS " + PW + "\r\n").encode())
    for line in ("CAP LS 302", "NICK e2e", "USER e2e 0 * :e2e"):
        s.sendall(line.encode() + b"\r\n")
    s.sendall(b"CAP END\r\n")
    print("--- registration:", " ".join(l for l in drain(s, 3.0).splitlines()[:3]))
    return s


obs = observer()
b = bouncer()
if MODE == "znc":
    b.sendall(b"PRIVMSG *simple_away :Timer\r\n")
    print("--- simple_away Timer ->", repr(" ".join(drain(b, 2.0).splitlines())))
    b.sendall(b"PRIVMSG *status :ListMods\r\n")
    mods = drain(b, 2.0)
    print("--- ListMods lines:", [l.strip() for l in mods.splitlines() if "simple_away" in l])

print("--- closing bouncer client")
b.close()
seen = drain(obs, 16.0)
interest = [l.strip() for l in seen.splitlines() if any(k in l for k in (" AWAY", " JOIN ", " PART ", " QUIT "))]
print("--- observer saw", len(seen.splitlines()), "lines; interesting:")
for l in interest:
    print("   ", l)
print("AWAY seen:", any(" AWAY" in l for l in seen.splitlines()))
obs.close()
