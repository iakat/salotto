#!/usr/bin/env python3
"""Where does a line typed into a front end actually stop?

The web UI echoed locally but its line was absent from the bouncer's replay ring.
There are two very different explanations and they need separating:

  (a) the front end never sent it  -> a UI bug, and the local echo is a lie;
  (b) the bouncer sent it upstream but does not buffer a client's own sends
      -> not a UI bug at all, and ZNC should behave the same way.

The way to tell is to watch the IRCd itself: if the line reached ergo, the
network saw it, whoever buffered it or not. Watching ergo rather than the bouncer
is the whole point.

Run against salotto and against ZNC in the same run, so the answer to (b) is
measured instead of assumed.
"""
import re
import socket
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

WEB = "http://salotto-ui:8080/"
SALOTTO = ("salotto-e2e", 6667)
ZNC = ("salotto-e2e-znc", 6667)
ERGO = ("salotto-e2e-ergo", 6667)
CHANNEL = "#e2e"


class Observer:
    """A plain client on ergo that watches a channel for someone else's PRIVMSG."""

    def __init__(self, nick):
        self.nick = nick
        self.sock = socket.create_connection(ERGO, 5)
        self.sock.settimeout(0.5)
        self.buf = b""
        self.lines = []
        self.stop = False
        self._thread = threading.Thread(target=self._read, daemon=True)

    def start(self):
        self.send("NICK " + self.nick)
        self.send("USER %s 0 * :observer" % self.nick)
        time.sleep(1.0)
        self.send("JOIN " + CHANNEL)
        self._thread.start()
        return self

    def send(self, line):
        self.sock.sendall(line.encode() + b"\r\n")

    def _read(self):
        while not self.stop:
            try:
                chunk = self.sock.recv(8192)
                if not chunk:
                    return
                self.buf += chunk
                while b"\r\n" in self.buf:
                    line, self.buf = self.buf.split(b"\r\n", 1)
                    self.lines.append(line.decode("utf-8", "replace"))
            except socket.timeout:
                continue
            except OSError:
                return

    def seen(self, marker):
        return [l for l in list(self.lines) if marker in l and "PRIVMSG" in l]

    def close(self):
        self.stop = True
        try:
            self.sock.close()
        except OSError:
            pass


class Client:
    """A bouncer client, used to submit a line and to ask for a replay."""

    def __init__(self, addr, nick, password="e2e"):
        self.sock = socket.create_connection(addr, 5)
        self.sock.settimeout(1.0)
        self.buf = b""
        self.send("PASS " + password)
        self.send("NICK " + nick)
        self.send("USER %s 0 * :%s" % (nick, nick))
        self.drain(1.5)

    def send(self, line):
        self.sock.sendall(line.encode() + b"\r\n")

    def drain(self, seconds):
        deadline = time.time() + seconds
        out = b""
        while time.time() < deadline:
            try:
                chunk = self.sock.recv(8192)
                if not chunk:
                    break
                out += chunk
            except socket.timeout:
                continue
        self.buf += out
        return out.decode("utf-8", "replace")

    def say(self, target, text):
        self.send("PRIVMSG %s :%s" % (target, text))
        self.drain(1.0)

    def replay(self, target):
        # ZNC spells it through *status, salotto takes the bare verb.
        self.send("PLAYBUFFER " + target)
        return self.drain(1.5)

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


def post_web(window, line):
    data = urllib.parse.urlencode({"window": window, "line": line}).encode()
    req = urllib.request.Request(WEB, data=data, method="POST")
    opener = urllib.request.build_opener(NoRedirect)
    try:
        return opener.open(req, timeout=10).status
    except urllib.error.HTTPError as e:
        return e.code


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **kw):
        return None


def main():
    obs = Observer("wirewatch").start()
    time.sleep(1.0)
    print("observer in", CHANNEL)

    results = {}

    # 1. A line typed into the deployed WEB FRONT END.
    marker_web = "from-web-%d" % int(time.time())
    post_web(CHANNEL, marker_web)
    time.sleep(2.0)
    seen_web = obs.seen(marker_web)
    print("web line reached ergo:", bool(seen_web))
    if seen_web:
        print("   ergo saw:", seen_web[0][:140])
    results["web"] = bool(seen_web)

    # 2. A line sent by a client connected DIRECTLY to salotto.
    marker_direct = "direct-%d" % int(time.time())
    sp = Client(SALOTTO, "e2e")
    sp.say(CHANNEL, marker_direct)
    time.sleep(2.0)
    seen_direct = obs.seen(marker_direct)
    print("direct salotto client reached ergo:", bool(seen_direct))
    results["direct_salotto"] = bool(seen_direct)

    # 3. Same, through ZNC. This is the control: if ZNC's line does not appear in
    #    ZNC's own replay either, then "own sends are not buffered" is a property
    #    of the bouncer model and not a salotto defect.
    marker_znc = "thru-znc-%d" % int(time.time())
    zp = Client(ZNC, "e2e")
    zp.say(CHANNEL, marker_znc)
    time.sleep(2.0)
    seen_znc = obs.seen(marker_znc)
    print("znc client reached ergo:", bool(seen_znc))
    results["znc"] = bool(seen_znc)

    # 4. Does each bouncer buffer its client's own line? Asked of the bouncers
    #    themselves, with the same question.
    sal_replay = sp.replay(CHANNEL)
    znc_replay = zp.replay(CHANNEL)
    sal_own = marker_direct in sal_replay
    znc_own = marker_znc in znc_replay
    print("salotto buffers its own send:", sal_own)
    print("znc buffers its own send:    ", znc_own)
    print("   -> parity:", sal_own == znc_own)

    # 5. And does the web line appear in the bouncer's replay, i.e. is it in the
    #    ring like any other line?
    print("web line in salotto replay:", marker_web in sal_replay)

    sp.close()
    zp.close()
    obs.close()

    ok = results["web"] and results["direct_salotto"]
    print()
    print("SUMMARY: the front end really sends" if results["web"] else "SUMMARY: THE FRONT END DID NOT SEND")
    print("         own sends buffered: salotto=%s znc=%s" % (sal_own, znc_own))
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
