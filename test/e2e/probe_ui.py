#!/usr/bin/env python3
"""Exercise the DEPLOYED front ends against the DEPLOYED bouncer.

This runs inside the cluster because that is the only place the UI Service and
the bouncer Service resolve, and it is deliberately not a mock: the point is to
prove the wiring works against a real bouncer, not that the model can be fed
literals.

Two traps this probe is built around, both of which produced false results before
it existed:

  * A UI can echo locally and send nothing, so "the line is on the page" proves
    nothing. The check that matters is whether the IRCd saw the line, so the
    probe watches ergo with a third-party nick and looks for the PRIVMSG.

  * A bouncer does NOT replay a client's own sends. Measured on both bouncers
    with a positive control (a received line IS replayed): salotto ignores its
    own sends exactly as ZNC 1.10.2 does. So PLAYBUFFER is the wrong place to
    look for a line the UI just sent, and an earlier version of this probe
    reported a bug that did not exist.

Usage: run as a pod in the same namespace as salotto-ui and salotto-e2e.
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
BOUNCER = ("salotto-e2e", 6667)
ERGO = ("salotto-e2e-ergo", 6667)
CHANNEL = "#e2e"
NICK = "e2e"
# salotto's e2e fixture has no client password; ZNC's does. A probe that assumes
# one password for both cannot tell "rejected" from "missing".
SALOTTO_PASSWORD = None


class Observer:
    """A plain client on ergo that watches the channel for other people's lines."""

    def __init__(self, nick):
        self.nick = nick
        self.sock = socket.create_connection(ERGO, 5)
        self.sock.settimeout(0.5)
        self.buf = b""
        self.lines = []
        self._stop = False
        self._thread = threading.Thread(target=self._read, daemon=True)

    def start(self):
        self._thread.start()
        self.send("NICK " + self.nick)
        self.send("USER %s 0 * :observer" % self.nick)
        time.sleep(0.8)
        self.send("JOIN " + CHANNEL)
        time.sleep(1.2)
        return self

    def send(self, line):
        self.sock.sendall(line.encode() + b"\r\n")

    def _read(self):
        while not self._stop:
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

    def privmsgs(self):
        return [l for l in list(self.lines) if "PRIVMSG" in l]

    def close(self):
        self._stop = True
        try:
            self.sock.close()
        except OSError:
            pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **kw):
        return None


def post_line(window, line):
    data = urllib.parse.urlencode({"window": window, "line": line}).encode()
    req = urllib.request.Request(WEB, data=data, method="POST")
    opener = urllib.request.build_opener(NoRedirect)
    try:
        return opener.open(req, timeout=10).status
    except urllib.error.HTTPError as e:
        return e.code


def page_text():
    with urllib.request.urlopen(WEB + "?width=110&height=26", timeout=10) as r:
        html = r.read().decode("utf-8", "replace")
    m = re.search(r'term-container">(.*?)</pre>', html, re.S)
    if not m:
        return ""
    text = re.sub(r"<[^>]+>", "", m.group(1))
    for a, b in (("&amp;", "&"), ("&lt;", "<"), ("&gt;", ">"), ("&#34;", '"'), ("&#39;", "'")):
        text = text.replace(a, b)
    return text


def main():
    ok = True
    stamp = int(time.time())

    obs = Observer("uiprobe%d" % (stamp % 1000)).start()
    print("observer joined", CHANNEL)

    # 1. The web front end accepts a line.
    marker = "ui-wire-%d" % stamp
    status = post_line(CHANNEL, marker)
    print("POST / ->", status)
    if status not in (303, 200):
        print("FAIL: the web front end rejected the line")
        ok = False

    # 2. The line really left the building: ergo saw it. This is the check a local
    #    echo cannot fake.
    time.sleep(2.0)
    seen = [l for l in obs.privmsgs() if marker in l]
    print("ergo saw the line:", bool(seen))
    if seen:
        print("  ", seen[0][:130])
    else:
        print("FAIL: the UI echoed locally and sent nothing")
        for l in obs.privmsgs()[-4:]:
            print("   ergo had:", l[:130])
        ok = False

    # 3. The page shows it in the window the UI is attached to.
    text = page_text()
    shows = marker in text
    print("page shows the line:", shows)
    if not shows:
        print("  page head:", text[:300].replace("\n", " | "))
        ok = False

    # 4. A bouncer command typed in the UI is answered by the bouncer, which proves
    #    the front end speaks the bouncer's dialect rather than a private one.
    post_line("status", "/listnetworks")
    time.sleep(1.5)
    text = page_text()
    answered = ("Network" in text and "Host" in text) or "ircd" in text
    print("bouncer answered a command in the UI:", answered)
    if not answered:
        print("  page head:", text[:400].replace("\n", " | "))
        ok = False

    obs.close()
    print("RESULT:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
