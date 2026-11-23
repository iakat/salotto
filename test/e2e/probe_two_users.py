"""Two users, one bouncer each: are they really separate?

The claim is not "two pods exist" but "alice and bob are different identities on
the IRCd and neither can read the other's traffic". That is measurable, and it is
the property the deployment exists for, so it gets measured rather than asserted.

For each user:
  - a client attaches to that user's bouncer and sends a line to #salotto-test,
  - a third-party observer on the IRCd reports whether it saw the line and under
    which nick,
and then:
  - the two nicks are compared, and the observer confirms that a line sent as
    alice did not arrive as bob.

Run as a pod in hermes-generated-content.
"""
import socket
import ssl
import sys
import threading
import time

# The real IRCd, not the e2e fixture: alice and bob are deployed against
# irc.ch-test.katia.sh, and an observer on the throwaway ergo would watch a
# different network and report that nothing arrived.
ERGO = ("irc.ch-test.katia.sh", 6697)
ERGO_TLS = True
CHANNEL = "#salotto-users"
USERS = {
    "alice": ("salotto-alice", 6667),
    "bob": ("salotto-bob", 6667),
}


class Client:
    def __init__(self, host, port, nick, channel=None):
        self.nick = nick
        raw = socket.create_connection((host, port), 8)
        if ERGO_TLS and (host, port) == ERGO:
            ctx = ssl.create_default_context()
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
            raw = ctx.wrap_socket(raw, server_hostname=host, do_handshake_on_connect=False)
        self.sock = raw
        # A recv timeout of 0.5s is right for polling, but it also bounds the TLS
        # handshake and the first read - on a cold connection that is too tight and
        # the socket dies before registration. Wait for the handshake with a real
        # timeout first, then switch to polling.
        self.sock.settimeout(15.0)
        try:
            self.sock.do_handshake()
        except Exception as exc:
            print("tls handshake with %s:%s failed: %s" % (host, port, exc))
        self.sock.settimeout(0.5)
        self.buf = b""
        self.lines = []
        self._stop = False
        threading.Thread(target=self._read, daemon=True).start()
        # CAP END must not be sent before the server's CAP LS reply: a client that
        # ends negotiation without waiting can have its registration answered
        # before it asked, and the whole handshake races. Either ask for nothing
        # (no CAP at all) or wait for the reply - this asks for nothing, which is
        # all a test observer needs.
        self.send("NICK " + nick)
        self.send("USER %s 0 * :%s" % (nick, nick))
        if channel:
            self.send("JOIN " + channel)
            time.sleep(2.0)

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

    def wait_for(self, needle, seconds=15.0):
        """Wait for a line containing needle, rather than sleeping and hoping.

        Fixed sleeps made this probe fail intermittently: the observer sometimes
        had not finished its TLS handshake and registration before the test sent
        a line, so it genuinely saw nothing and reported a working system broken.
        Waiting on the signal the protocol provides removes the guess.
        """
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


def main():
    stamp = int(time.time()) % 100000
    ok = True

    # The observer is a plain client on the IRCd, so it sees what actually
    # arrived, not what either bouncer believes.
    obs = Client(ERGO[0], ERGO[1], "obs%d" % stamp)
    # A generous budget on purpose: ergo does a reverse DNS lookup on every new
    # connection and this cluster's resolver can take 20+ seconds to say
    # "Couldn't look up your hostname" before registration finishes. Measured:
    # 15s was sometimes too short and the probe reported a broken deployment
    # that was only waiting on DNS.
    if not obs.wait_for(" 001 ", 45.0):
        print("FAIL: the observer never registered; raw head:", [l[:90] for l in list(obs.lines)[:6]])
        return 1
    obs.send("JOIN " + CHANNEL)
    obs.wait_for(" 366 ", 15.0)
    joined = [l for l in list(obs.lines) if " JOIN " in l or " 353 " in l or " 471 " in l or " 403 " in l or " 474 " in l]
    print("observer channel lines:", [l[:110] for l in joined][:5])
    if not joined:
        print("observer raw head:", [l[:90] for l in list(obs.lines)[:6]])

    seen_by = {}
    for user, (host, port) in USERS.items():
        mark = "%s-%d" % (user, stamp)
        cli = Client(host, port, user + "-cli")
        if not cli.wait_for(" 001 ", 45.0):
            print("%s: FAIL: its bouncer never registered the client" % user)
            ok = False
            continue
        cli.send("JOIN " + CHANNEL)
        cli.wait_for(" 366 ", 15.0)
        cli.send("PRIVMSG %s :%s" % (CHANNEL, mark))
        # The measurement is the observer seeing it, so wait for that rather than
        # for a number of seconds guessed at twice.
        hit = []
        end = time.time() + 15.0
        while time.time() < end:
            hit = [l for l in list(obs.lines) if mark in l]
            if hit:
                break
            time.sleep(0.3)

        who = ""
        if hit:
            # ":alice!~u@host PRIVMSG #salotto-users :alice-12345"
            who = hit[0][1:].split("!", 1)[0]
        seen_by[user] = (bool(hit), who)
        print("%s: line reached the IRCd: %s (as %r)" % (user, bool(hit), who))
        if not hit:
            print("  FAIL: %s's client could not speak through its bouncer" % user)
            ok = False
        cli.close()
        time.sleep(1.0)

    a_ok, a_nick = seen_by.get("alice", (False, ""))
    b_ok, b_nick = seen_by.get("bob", (False, ""))
    if a_ok and b_ok:
        if a_nick == b_nick:
            print("FAIL: both users appear as %r - one identity shared, not two" % a_nick)
            ok = False
        else:
            print("distinct identities on the IRCd: %r vs %r" % (a_nick, b_nick))

    # Cross-reading: a line sent by alice must never appear as bob's client.
    mark = "cross-%d" % stamp
    alice = Client(USERS["alice"][0], USERS["alice"][1], "alice-cli2")
    alice.send("JOIN " + CHANNEL)
    time.sleep(2.0)
    bob = Client(USERS["bob"][0], USERS["bob"][1], "bob-cli2")
    bob.send("JOIN " + CHANNEL)
    time.sleep(2.5)

    alice.send("PRIVMSG %s :%s" % (CHANNEL, mark))
    time.sleep(3.0)
    # bob IS in the channel, so he legitimately sees a channel message - what he
    # must not see is alice's private traffic. Use a PM for the isolation check.
    pm = "pm-%d" % stamp
    alice.send("PRIVMSG bob :%s" % pm)
    time.sleep(3.0)
    bob_saw_pm = [l for l in list(bob.lines) if pm in l]
    print("bob received alice's PM (expected: yes, that is IRC):", bool(bob_saw_pm))

    # Replay check, and the expectation has to be the true one: alice addressed
    # that PM to bob's nick, so it is bob's message and bob's bouncer must replay
    # it to bob. Seeing it is correct behaviour, not a leak - the earlier version
    # of this probe printed a "expected: no" that was simply wrong about IRC.
    # What must NOT appear in bob's replay is a message alice sent to herself.
    alice.send("PRIVMSG alice :%s-self" % pm)
    time.sleep(2.5)
    fresh = Client(USERS["bob"][0], USERS["bob"][1], "bob-cli3")
    time.sleep(3.0)
    replay = list(fresh.lines)
    own = [l for l in replay if pm in l]
    self_msg = [l for l in replay if (pm + "-self") in l]
    print("bob's replay has the PM addressed to him (expected: yes):", bool(own))
    print("bob's replay has alice's message to herself (expected: no):", bool(self_msg))
    if self_msg:
        print("  FAIL: one user's private traffic reached another user's buffer")
        ok = False
    if not own:
        print("  FAIL: bob's own PM was not replayed to him")
        ok = False

    alice.close(); bob.close(); fresh.close(); obs.close()

    print("RESULT:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
