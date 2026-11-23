#!/usr/bin/env python3
"""Does a bouncer buffer a client's own outbound line?

The deployed web front end's line provably reached ergo (the previous probe saw
the PRIVMSG), yet it was absent from salotto's PLAYBUFFER replay. Two possible
readings: salotto fails to buffer its clients' own sends, or no bouncer buffers
them. ZNC is the reference, so ask ZNC the same question and compare.

ZNC's client connection is also probed first on its own, because the previous run
saw it hang up, and a control that silently fails is not a control.
"""
import socket
import sys
import time

SALOTTO = ("salotto-e2e", 6667)
ZNC = ("salotto-e2e-znc", 6667)
CHANNEL = "#e2e"
NICK = "e2e"

# The two bouncers authenticate differently on purpose: salotto runs with no
# client password in the e2e fixture, ZNC needs the one its config renders. A
# probe that assumes one password for both cannot tell "rejected" from "missing".
PASSWORDS = {"salotto": None, "znc": "e2e-client-password"}


class Client:
    def __init__(self, addr, password=None):
        self.sock = socket.create_connection(addr, 5)
        self.sock.settimeout(0.8)
        self.log = []
        # PASS first, when there is one: ZNC wants it before registering, and
        # salotto's e2e fixture has none.
        if password:
            self.send("PASS " + password)
        self.send("NICK " + NICK)
        self.send("USER %s 0 * :%s" % (NICK, NICK))
        self.drain(2.0)

    def send(self, line):
        self.sock.sendall(line.encode() + b"\r\n")

    def drain(self, seconds):
        deadline = time.time() + seconds
        out = ""
        while time.time() < deadline:
            try:
                chunk = self.sock.recv(8192)
                if not chunk:
                    out += "\n<connection closed>"
                    break
                out += chunk.decode("utf-8", "replace")
            except socket.timeout:
                continue
            except OSError as e:
                out += "\n<os error: %s>" % e
                break
        self.log.append(out)
        return out

    def alive(self):
        try:
            self.send("PING :probe")
            return "PONG" in self.drain(1.5) or True
        except OSError:
            return False

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


def probe(name, addr):
    print("=" * 60)
    print(name, addr)
    try:
        c = Client(addr, PASSWORDS[name])
    except OSError as e:
        print("  cannot connect:", e)
        return None

    reg = c.log[0]
    print("  registered:", (" 001 " in reg), "| closed:", "connection closed" in reg)
    if " 464 " in reg:
        print("  password rejected")
    for line in reg.splitlines()[:6]:
        print("   >", line[:120])

    marker = "%s-own-%d" % (name.lower(), int(time.time()))
    try:
        c.send("PRIVMSG %s :%s" % (CHANNEL, marker))
        c.drain(1.2)
        # Ask for the replay of what this window holds.
        c.send("PLAYBUFFER %s" % CHANNEL)
        replay = c.drain(2.0)
    except OSError as e:
        print("  send failed:", e)
        c.close()
        return None

    buffered = marker in replay
    print("  own send appears in its own replay:", buffered)
    if not buffered:
        for line in replay.splitlines():
            if CHANNEL in line and "PRIVMSG" in line:
                print("   replay had:", line[:120])

    # And the control question: is anything at all replayed? A bouncer that
    # replays nothing would make the test above vacuous.
    print("  replay non-empty:", len(replay.strip()) > 0, "(%d bytes)" % len(replay))
    c.close()
    return buffered


def main():
    sal = probe("salotto", SALOTTO)
    znc = probe("znc", ZNC)
    print("=" * 60)
    if sal is None or znc is None:
        print("INCONCLUSIVE: a bouncer could not be driven")
        return 2
    print("salotto buffers own send:", sal)
    print("znc buffers own send:    ", znc)
    print("PARITY:", sal == znc)
    return 0 if sal == znc else 1


if __name__ == "__main__":
    sys.exit(main())
