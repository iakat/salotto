#!/usr/bin/env python3
"""Drive the deployed SSH front end with a real SSH client.

The web probe can post HTTP; the SSH front end has no such surface, so the only
honest test is to be an SSH client. The screen is a terminal UI, so the check is
whether a line that arrives at the bouncer is rendered in the session's screen —
and the line is injected through the bouncer, not through the UI, which makes the
direction unambiguous: bouncer -> transport -> model -> SSH session.

A line injected through the bouncer is also the only way to test this without
typing into a pty, and it is the direction that was broken before the transport
existed.
"""
import os
import re
import socket
import sys
import threading
import time

import paramiko

WEB_NS = os.environ.get("POD_NAMESPACE", "hermes-generated-content")
SSH_HOST = "salotto-ui"
SSH_PORT = 2222
BOUNCER = ("salotto-e2e", 6667)
ERGO = ("salotto-e2e-ergo", 6667)
CHANNEL = "#e2e"

ANSI = re.compile(rb"\x1b\[[0-9;?]*[a-zA-Z]|\x1b[()][A-Z0-9]|\x1b[=>]")


def strip_ansi(raw: bytes) -> str:
    return ANSI.sub(b"", raw).decode("utf-8", "replace")


class Speaker:
    """A plain client on ergo, to put a line in the channel from outside."""

    def __init__(self, nick):
        self.sock = socket.create_connection(ERGO, 5)
        self.sock.settimeout(0.5)
        self.buf = b""
        threading.Thread(target=self._read, daemon=True).start()
        self.send("NICK " + nick)
        self.send("USER %s 0 * :speaker" % nick)
        time.sleep(0.8)
        self.send("JOIN " + CHANNEL)
        time.sleep(1.2)

    def send(self, line):
        self.sock.sendall(line.encode() + b"\r\n")

    def _read(self):
        while True:
            try:
                if not self.sock.recv(8192):
                    return
            except socket.timeout:
                continue
            except OSError:
                return


def main():
    ok = True
    stamp = int(time.time())

    client = paramiko.SSHClient()
    client.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    try:
        # A password must be offered: the server's handler accepts any password
        # when none is configured, but it cannot authenticate a client that
        # offers none at all - paramiko then reports "No authentication methods
        # available" and the failure looks like a broken server.
        client.connect(SSH_HOST, port=SSH_PORT, username="probe",
                       password="probe", look_for_keys=False,
                       allow_agent=False, timeout=10)
    except Exception as e:
        print("FAIL: could not open an SSH session:", e)
        return 1
    print("ssh session open")

    chan = client.invoke_shell(term="xterm-256color", width=110, height=30)
    chan.settimeout(0.5)
    screen = bytearray()

    def pump(seconds):
        deadline = time.time() + seconds
        while time.time() < deadline:
            try:
                chunk = chan.recv(65536)
                if not chunk:
                    return
                screen.extend(chunk)
            except socket.timeout:
                continue

    pump(3.0)
    print("initial screen bytes:", len(screen))
    if not screen:
        print("FAIL: the SSH session rendered nothing")
        ok = False

    # The window the front end opens by default is #e2e (its -window flag). Put a
    # line in that channel from ergo and see it arrive on the screen.
    marker = "ssh-arrival-%d" % stamp
    sp = Speaker("spk%d" % (stamp % 1000))
    sp.send("PRIVMSG %s :%s" % (CHANNEL, marker))
    pump(4.0)

    text = strip_ansi(bytes(screen))
    arrived = marker in text
    print("line from the bouncer reached the SSH screen:", arrived)
    if not arrived:
        print("  screen tail:", text[-400:].replace("\n", " | "))
        ok = False

    # Colours: a terminal UI that lost its profile renders monochrome, which is
    # the classic daemon-has-no-TTY failure.
    has_sgr = b"\x1b[38;5;" in bytes(screen)
    print("256-colour SGR present:", has_sgr)
    if not has_sgr:
        print("FAIL: no 256-colour output, the profile was not pinned")
        ok = False

    # Sending: type into the shell and confirm it reaches ergo. This is the
    # direction the web probe covers with an HTTP POST.
    out = "ssh-send-%d" % stamp
    obs = socket.create_connection(ERGO, 5)
    obs.settimeout(0.5)
    seen = bytearray()
    threading.Thread(target=lambda: _pump_into(obs, seen), daemon=True).start()
    for line in ("NICK obs%d" % (stamp % 1000), "USER o 0 * :o", "JOIN " + CHANNEL):
        obs.sendall(line.encode() + b"\r\n")
        time.sleep(0.4)
    time.sleep(1.2)
    seen.clear()

    # Ctrl-N would move windows, so type straight into the window the session
    # opened on.
    chan.send(out + "\r")
    pump(3.0)
    time.sleep(1.5)
    got = out in seen.decode("utf-8", "replace")
    print("typed line reached ergo:", got)
    if not got:
        print("  observer saw:", seen.decode("utf-8", "replace")[:300].replace("\r\n", " | "))
        ok = False

    chan.close()
    client.close()
    sp.sock.close()
    obs.close()
    print("RESULT:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


def _pump_into(sock, buf):
    while True:
        try:
            chunk = sock.recv(8192)
            if not chunk:
                return
            buf.extend(chunk)
        except socket.timeout:
            continue
        except OSError:
            return


if __name__ == "__main__":
    sys.exit(main())
