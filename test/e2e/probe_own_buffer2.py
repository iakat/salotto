#!/usr/bin/env python3
"""Own-send buffering, with the positive control the last probe was missing.

The previous run concluded "neither bouncer buffers its own client's sends",
which is plausible but was measured against a ZNC replay of 49 bytes — that may
have been an empty buffer rather than a bouncer that chose not to store the line.
A test that compares two silences proves nothing.

So this probe establishes, for each bouncer, that PLAYBUFFER works at all: a
third-party nick says something in the channel, and the bouncer must replay it.
Only once that control passes is the own-send answer meaningful.
"""
import socket
import sys
import threading
import time

SALOTTO = ("salotto-e2e", 6667)
ZNC = ("salotto-e2e-znc", 6667)
ERGO = ("salotto-e2e-ergo", 6667)
CHANNEL = "#e2e"
NICK = "e2e"
PASSWORDS = {"salotto": None, "znc": "e2e-client-password"}


class Party:
    """A plain IRC client on ergo, used to speak into the channel."""

    def __init__(self, nick):
        self.nick = nick
        self.sock = socket.create_connection(ERGO, 5)
        self.sock.settimeout(0.5)
        self.buf = b""
        self.ready = threading.Event()
        self.thread = threading.Thread(target=self._read, daemon=True)

    def start(self):
        self.thread.start()
        self.send("NICK " + self.nick)
        self.send("USER %s 0 * :party" % self.nick)
        time.sleep(0.8)
        self.send("JOIN " + CHANNEL)
        time.sleep(1.2)
        return self

    def send(self, line):
        self.sock.sendall(line.encode() + b"\r\n")

    def _read(self):
        while True:
            try:
                chunk = self.sock.recv(8192)
                if not chunk:
                    return
                self.buf += chunk
            except socket.timeout:
                continue
            except OSError:
                return

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


class Bouncer:
    def __init__(self, name, addr):
        self.name = name
        self.sock = socket.create_connection(addr, 5)
        self.sock.settimeout(0.8)
        pw = PASSWORDS[name]
        if pw:
            self.send("PASS " + pw)
        self.send("NICK " + NICK)
        self.send("USER %s 0 * :%s" % (NICK, NICK))
        reg = self.drain(2.5)
        self.registered = " 001 " in reg

    def send(self, line):
        self.sock.sendall(line.encode() + b"\r\n")

    def drain(self, seconds):
        deadline = time.time() + seconds
        out = ""
        while time.time() < deadline:
            try:
                chunk = self.sock.recv(32768)
                if not chunk:
                    break
                out += chunk.decode("utf-8", "replace")
            except socket.timeout:
                continue
            except OSError:
                break
        return out

    def replay(self, target):
        self.send("PLAYBUFFER " + target)
        return self.drain(2.5)

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


def main():
    stamp = int(time.time())
    peer = Party("peer%d" % (stamp % 1000)).start()

    results = {}
    for name, addr in (("salotto", SALOTTO), ("znc", ZNC)):
        print("=" * 60)
        print(name)
        try:
            b = Bouncer(name, addr)
        except OSError as e:
            print("  cannot connect:", e)
            results[name] = None
            continue
        print("  registered:", b.registered)
        # Clear whatever is in the buffer so the replay below is unambiguous.
        b.send("CLEARBUFFER " + CHANNEL)
        b.drain(0.6)
        if name == "znc":
            # ZNC spells commands through *status.
            b.send("PRIVMSG *status :ClearBuffer " + CHANNEL)
            b.drain(0.8)

        # The control: a line from someone else must be replayed.
        incoming = "%s-in-%d" % (name, stamp)
        peer.send("PRIVMSG %s :%s" % (CHANNEL, incoming))
        time.sleep(1.5)
        rep = b.replay(CHANNEL)
        got_incoming = incoming in rep
        print("  control: a received line is replayed:", got_incoming, "(%d bytes)" % len(rep))

        # The question: our own line.
        own = "%s-own-%d" % (name, stamp)
        b.send("PRIVMSG %s :%s" % (CHANNEL, own))
        b.drain(1.0)
        b.send("CLEARBUFFER " + CHANNEL)
        b.drain(0.5)
        if name == "znc":
            b.send("PRIVMSG *status :ClearBuffer " + CHANNEL)
            b.drain(0.6)
        rep2 = b.replay(CHANNEL)
        got_own = own in rep2
        print("  own line is replayed:", got_own, "(%d bytes)" % len(rep2))

        results[name] = (got_incoming, got_own)
        b.close()

    peer.close()
    print("=" * 60)
    for name, r in results.items():
        print("%-8s control=%s own=%s" % (name, r[0] if r else "?", r[1] if r else "?"))
    if any(r is None for r in results.values()):
        print("INCONCLUSIVE")
        return 2
    if not all(r[0] for r in results.values()):
        print("INCONCLUSIVE: a control failed, so the own-send answer is not meaningful")
        return 2
    parity = results["salotto"][1] == results["znc"][1]
    print("PARITY on own sends:", parity)
    return 0 if parity else 1


if __name__ == "__main__":
    sys.exit(main())
