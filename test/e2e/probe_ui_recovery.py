"""Does the web front end survive its bouncer being killed and come back working?

This is the deployed counterpart to exit_test.go, which proves the process exits.
Exiting is only half the claim: the interesting question is whether the system
recovers, and only the cluster can answer that. The probe

  1. checks the page is alive and its input really reaches the IRCd,
  2. deletes the bouncer pod underneath the front end,
  3. waits for the front end to come back on its own, and
  4. repeats step 1 - the same measurement, not a weaker one.

A front end that comes back but whose input goes nowhere fails step 4, which is
the failure this whole change is about.

Run as a pod in hermes-generated-content with RBAC to delete pods.
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
ERGO = ("salotto-e2e-ergo", 6667)
CHANNEL = "#e2e"


class Observer:
    def __init__(self, nick):
        self.nick = nick
        self.sock = socket.create_connection(ERGO, 5)
        self.sock.settimeout(0.5)
        self.buf = b""
        self.lines = []
        self._stop = False
        threading.Thread(target=self._read, daemon=True).start()

    def start(self):
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
        return list(self.lines)

    def close(self):
        self._stop = True
        try:
            self.sock.close()
        except OSError:
            pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **kw):
        return None


def post_line(window, line, timeout=10):
    data = urllib.parse.urlencode({"window": window, "line": line}).encode()
    req = urllib.request.Request(WEB, data=data, method="POST")
    opener = urllib.request.build_opener(NoRedirect)
    try:
        return opener.open(req, timeout=timeout).status
    except urllib.error.HTTPError as e:
        return e.code
    except Exception:
        return 0


def page_text(timeout=10):
    try:
        with urllib.request.urlopen(WEB + "?width=110&height=26", timeout=timeout) as r:
            html = r.read().decode("utf-8", "replace")
    except Exception:
        return ""
    m = re.search(r'term-container">(.*?)</pre>', html, re.S)
    if not m:
        return ""
    text = re.sub(r"<[^>]+>", "", m.group(1))
    for a, b in (("&amp;", "&"), ("&lt;", "<"), ("&gt;", ">"), ("&#34;", '"'), ("&#39;", "'")):
        text = text.replace(a, b)
    return text


def wait_http(budget):
    """Wait until the page answers again, which is what a supervisor restart looks
    like from outside."""
    end = time.time() + budget
    while time.time() < end:
        if page_text(timeout=4):
            return True
        time.sleep(2.0)
    return False


def line_reaches_ergo(obs, tag):
    """The end-to-end measurement: a line typed in the browser must reach the IRCd.

    A local echo cannot fake this because the observer is a third client on ergo.
    """
    for _ in range(3):
        post_line(CHANNEL, tag)
        time.sleep(2.5)
        if [l for l in obs.privmsgs() if tag in l]:
            return True
        time.sleep(2.0)
    return False


def main():
    # The kill itself is done from outside this pod, by whoever runs the probe:
    # the probe's service account cannot delete pods and should not be able to.
    # This process watches for the resulting outage and then measures whether the
    # front end came back with a working transport, which is the actual claim.
    stamp = int(time.time())
    obs = Observer("uires%d" % (stamp % 10000)).start()
    print("observer joined", CHANNEL, flush=True)

    ok = True
    before = "ui-before-%d" % stamp
    if not line_reaches_ergo(obs, before):
        print("FAIL: before the restart, the UI's input did not reach the IRCd")
        ok = False
    else:
        print("before restart: input reaches the IRCd", flush=True)

    # Announce the moment to kill: the driver deletes the bouncer pod on seeing
    # this line, so the outage lands inside the window this process is watching.
    print("READY-FOR-KILL", flush=True)

    # Watch for the outage. The front end must become unavailable when the
    # bouncer goes away - that is the visible half of "it noticed" - and then come
    # back without help.
    gone_at = None
    back_at = None
    deadline = time.time() + 240.0
    while time.time() < deadline:
        if gone_at is None:
            if not page_text(timeout=3):
                gone_at = time.time()
                print("front end went down", flush=True)
        else:
            if page_text(timeout=4):
                back_at = time.time()
                break
        time.sleep(2.0)

    if gone_at is None:
        print("FAIL: the front end never noticed the bouncer going away")
        obs.close()
        return 1
    if back_at is None:
        print("FAIL: the front end went down and never came back")
        obs.close()
        return 1
    print("front end came back after %.0fs" % (back_at - gone_at), flush=True)

    # A front end can answer while its transport is still dead, so let it settle
    # and then repeat the exact measurement from before. This is the step that
    # distinguishes "the process restarted" from "the front end works again".
    time.sleep(12)
    after = "ui-after-%d" % stamp
    if not line_reaches_ergo(obs, after):
        print("FAIL: after the restart, the UI answers but its input goes nowhere")
        ok = False
    else:
        print("after restart: input reaches the IRCd")

    obs.close()
    print("RESULT:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
