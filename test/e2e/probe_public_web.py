"""Does typing into the PUBLIC alice page actually reach the IRCd?

Everything so far proved the pieces separately: the page returns 200, the bouncer
holds alice on the IRCd, and the e2e fixture's web front end sends. None of that
is the thing a user does. The thing a user does is open the URL, type, and have
the line arrive - through the edge Gateway, through the deployed daemon, out to
the real ergo.

So this probe drives the public hostname only:
  1. POST /join to make a fresh channel, because alice joins nothing by default
     and a message to a channel she is not in is a 404 from the IRCd, not a test.
  2. POST a message to that channel window.
  3. A third-party observer on the IRCd reports whether it arrived and under
     which nick - a local echo cannot fake that.
  4. GET the page and check the line is rendered back.

Run as a pod in hermes-generated-content; it needs only outbound HTTPS and IRC.
"""
import re
import socket
import ssl
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request

USERS = ["alice", "bob"]
IRC = ("irc.ch-test.katia.sh", 6697)
CHANNEL = "#salotto-webcheck"


class Observer:
    def __init__(self, nick):
        self.nick = nick
        raw = socket.create_connection(IRC, 15)
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        self.sock = ctx.wrap_socket(raw, server_hostname=IRC[0],
                                    do_handshake_on_connect=False)
        self.sock.settimeout(20.0)
        self.sock.do_handshake()
        self.sock.settimeout(0.5)
        self.buf = b""
        self.lines = []
        self._stop = False
        threading.Thread(target=self._read, daemon=True).start()
        self.send("NICK " + nick)
        self.send("USER %s 0 * :%s" % (nick, nick))

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

    def wait_for(self, needle, seconds=60.0):
        end = time.time() + seconds
        while time.time() < end:
            for line in list(self.lines):
                if needle in line:
                    return True
            time.sleep(0.2)
        return False

    def close(self):
        self._stop = True
        try:
            self.sock.close()
        except OSError:
            pass


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *a, **kw):
        return None


def base(user):
    return "https://%s.ch-test.katia.sh/" % user


def post(user, window, line):
    data = urllib.parse.urlencode({"window": window, "line": line}).encode()
    req = urllib.request.Request(base(user), data=data, method="POST")
    opener = urllib.request.build_opener(NoRedirect)
    try:
        return opener.open(req, timeout=20).status
    except urllib.error.HTTPError as e:
        return e.code
    except Exception as e:
        print("  POST failed: %s" % e)
        return 0


def page(user):
    try:
        with urllib.request.urlopen(base(user) + "?width=120&height=30", timeout=20) as r:
            html = r.read().decode("utf-8", "replace")
    except Exception as e:
        print("  GET failed: %s" % e)
        return ""
    m = re.search(r'term-container">(.*?)</pre>', html, re.S)
    if not m:
        return ""
    text = re.sub(r"<[^>]+>", "", m.group(1))
    for a, b in (("&amp;", "&"), ("&lt;", "<"), ("&gt;", ">"), ("&#34;", '"'), ("&#39;", "'")):
        text = text.replace(a, b)
    return text


def check(user, obs, stamp):
    """One user, end to end, through the public URL."""
    ok = True
    # Join first: alice joins nothing by default, so a message to an unjoined
    # channel is an IRCd 404 rather than a test of the front end.
    post(user, "status", "/join " + CHANNEL)
    time.sleep(4.0)
    text = page(user)
    if CHANNEL not in text:
        print("  page does not show %s after /join" % CHANNEL)

    mark = "%s-web-%d" % (user, stamp)
    st = post(user, CHANNEL, mark)
    print("  POST ->", st)

    # The measurement that matters: a third party on the real IRCd.
    hit = []
    end = time.time() + 30.0
    while time.time() < end:
        hit = [l for l in list(obs.lines) if mark in l]
        if hit:
            break
        time.sleep(0.5)
    if hit:
        # ":alice!~u@host PRIVMSG #salotto-webcheck :alice-web-123"
        who = hit[0][1:].split("!", 1)[0] if hit[0].startswith(":") else "?"
        print("  IRCd saw it as %r" % who)
        if who.lower() != user:
            print("  FAIL: sent as %s but arrived as %s" % (user, who))
            ok = False
    else:
        print("  FAIL: the line typed into the public page never reached the IRCd")
        ok = False

    # And the page shows the user's own line back.
    text = page(user)
    if mark not in text:
        print("  FAIL: the page does not show the line it just sent")
        ok = False
    else:
        print("  page renders its own line: yes")
    return ok


def main():
    stamp = int(time.time()) % 100000
    obs = Observer("webobs%d" % stamp)
    if not obs.wait_for(" 001 ", 90.0):
        print("FAIL: observer never registered; head:",
              [l[:90] for l in list(obs.lines)[:6]])
        return 1
    obs.send("JOIN " + CHANNEL)
    obs.wait_for(" 366 ", 30.0)

    ok = True
    for user in USERS:
        print("%s:" % user)
        if not check(user, obs, stamp):
            ok = False

    obs.close()
    print("RESULT:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
