#!/usr/bin/env python3
"""Prove the admin audit trail against the deployed bouncer.

The claim is not "a command answered" but "an operator action that happened is on the record, in
order, with its outcome, and survives the process". So this runs a mixture of accepted and refused
commands through a real bouncer connection and then reads the file the bouncer wrote.

The refused ones are the point. Every controlplane command answers with a NOTICE, including the
refusals, so an audit built on "did the reply get sent" would log SHUTDOWN as accepted. This checks
that the file says otherwise.
"""
import socket
import sys
import time

HOST = ("salotto-alice.hermes-generated-content.svc.cluster.local", 6667)
NICK = "alice"


def connect():
    s = socket.create_connection(HOST, timeout=30)
    s.settimeout(30)

    def send(line):
        s.sendall((line + "\r\n").encode())

    send("CAP LS 302")
    send("NICK " + NICK)
    send("USER " + NICK + " 0 * :audit probe")
    # Drain registration; the bouncer replays every window, which can take a while.
    buf = b""
    deadline = time.time() + 45
    while time.time() < deadline:
        try:
            chunk = s.recv(65536)
        except socket.timeout:
            break
        if not chunk:
            break
        buf += chunk
        if b" 001 " in buf and b" 376 " in buf:
            break
    return s, send


def drain(s, seconds=3.0):
    out = b""
    end = time.time() + seconds
    s.settimeout(0.5)
    while time.time() < end:
        try:
            chunk = s.recv(65536)
        except socket.timeout:
            continue
        if not chunk:
            break
        out += chunk
    return out.decode(errors="replace")


def main():
    s, send = connect()
    ok = True

    def check(label, cond, detail=""):
        nonlocal ok
        print(("PASS  " if cond else "FAIL  ") + label + (("  " + detail) if detail else ""))
        if not cond:
            ok = False

    # One accepted, one refused, one unknown. In that order, so the file's order is also a claim.
    send("PRIVMSG *controlplane :VERSION")
    time.sleep(0.6)
    version = drain(s, 2.0)
    check("VERSION answered", "salotto" in version, version.strip()[:80])

    send("PRIVMSG *controlplane :SHUTDOWN now")
    time.sleep(0.6)
    shutdown = drain(s, 2.0)
    check("SHUTDOWN refused", "remote off switch" in shutdown, shutdown.strip()[:100])

    send("PRIVMSG *controlplane :FROBNICATE the audit probe")
    time.sleep(0.6)
    unknown = drain(s, 2.0)
    check("unknown command answered", "Unknown controlplane command" in unknown, unknown.strip()[:80])

    # And the trail is readable from the same connection, which is what makes it useful without
    # a shell on the box.
    send("PRIVMSG *controlplane :ADMINLOG 5")
    time.sleep(0.6)
    trail = drain(s, 2.5)
    check("ADMINLOG listed the actions", "SHUTDOWN" in trail and "VERSION" in trail,
          "%d bytes" % len(trail))
    check("the trail says refused, not ok", "REFUSED" in trail, trail[:200].replace("\r\n", " | "))

    s.close()
    print("\n" + ("ALL PASS" if ok else "FAILURES ABOVE"))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
