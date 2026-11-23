"""Does a line typed into the public alice page reach the IRCd, live?

The witness problem: an echo in alice's own render proves only that the server can
render. The honest witness is a *different* process attached to the same IRCd, so
this probe drives both public front ends at once -

  alice's browser joins a channel and sends a line over the WebSocket;
  bob's browser joins the same channel and must see it arrive.

Bob's bouncer is a separate process with its own upstream connection, so the only
way that line gets into bob's render is by crossing ergo. That is the measurement.

An earlier version registered a raw observer socket instead. It worked, then
stopped: ergo's connect-ip handler began dropping the probe's unregistered
sessions outright, most likely connection throttling triggered by this project's
own probe traffic from one node IP. Rather than tune a rate limit to make a test
pass, the probe now witnesses through a client that is part of the deployment,
which is also closer to how a user would check.

Run as a pod in hermes-generated-content.
"""
import base64
import json
import os
import socket
import ssl
import struct
import sys
import time

HOSTS = {"alice": "alice.ch-test.katia.sh", "bob": "bob.ch-test.katia.sh"}
CHANNEL = "#salotto-wscheck"


class WS:
    """A minimal WebSocket client.

    Hand-rolled on purpose: a library that transparently reconnects would hide a
    proxy that closes the socket every 30 seconds, and this probe exists partly to
    find out whether the edge proxy does that.
    """

    def __init__(self, host):
        self.host = host
        ctx = ssl.create_default_context()
        sock = socket.create_connection((host, 443), 20)
        sock.settimeout(20)
        self.sock = ctx.wrap_socket(sock, server_hostname=host)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall((
            "GET /ws HTTP/1.1\r\n"
            "Host: %s\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            "Sec-WebSocket-Key: %s\r\n"
            "Sec-WebSocket-Version: 13\r\n\r\n" % (host, key)
        ).encode())
        buf = b""
        while b"\r\n\r\n" not in buf:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise RuntimeError("closed during handshake: %r" % buf[:200])
            buf += chunk
        head, self.buf = buf.split(b"\r\n\r\n", 1)
        status = head.split(b"\r\n")[0].decode()
        if "101" not in status:
            raise RuntimeError("handshake refused: %s" % status)

    def _exact(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RuntimeError("socket closed mid-frame")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def _frame(self):
        hdr = self._exact(2)
        fin, op = bool(hdr[0] & 0x80), hdr[0] & 0x0F
        masked = hdr[1] & 0x80
        ln = hdr[1] & 0x7F
        if ln == 126:
            ln = struct.unpack("!H", self._exact(2))[0]
        elif ln == 127:
            ln = struct.unpack("!Q", self._exact(8))[0]
        mask = self._exact(4) if masked else b""
        payload = self._exact(ln)
        if masked:
            payload = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        return fin, op, payload

    def send(self, obj):
        payload = json.dumps(obj).encode()
        mask = os.urandom(4)
        hdr = bytes([0x81])
        n = len(payload)
        if n < 126:
            hdr += bytes([0x80 | n])
        elif n < 65536:
            hdr += bytes([0x80 | 126]) + struct.pack("!H", n)
        else:
            hdr += bytes([0x80 | 127]) + struct.pack("!Q", n)
        self.sock.sendall(hdr + mask + bytes(b ^ mask[i % 4] for i, b in enumerate(payload)))

    def state(self, within=20.0, want=None):
        """Read until a state frame matching want.

        A message is not a frame: the server writes through a 4 KB buffer, so a
        full render arrives as a first frame plus continuations. Reading one frame
        and calling json.loads on it fails with "Unterminated string" at about the
        buffer size, which is what made an earlier version of this probe lie.
        """
        end = time.time() + within
        pending, pending_op = b"", None
        while time.time() < end:
            self.sock.settimeout(max(0.5, end - time.time()))
            fin, op, payload = self._frame()
            if op == 0x8:
                raise RuntimeError("server closed the socket")
            if op in (0x9, 0xA):
                continue
            if op == 0x0:
                if pending_op is None:
                    continue
                pending += payload
                if not fin:
                    continue
                payload, pending, pending_op = pending, b"", None
            elif op == 0x1:
                if not fin:
                    pending, pending_op = payload, op
                    continue
            else:
                continue
            f = json.loads(payload)
            if f.get("type") == "state" and (want is None or want(f)):
                return f
        raise RuntimeError("no matching state frame within %.0fs" % within)

    def frame(self, within=20.0, want=None):
        """Read until a frame matching want, of ANY type.

        state() filters to state frames because that is what a render check wants.
        A search answer is a "results" frame, so a probe that can only see states
        reports a working search as a timeout.
        """
        end = time.time() + within
        pending, pending_op = b"", None
        while time.time() < end:
            self.sock.settimeout(max(0.5, end - time.time()))
            fin, op, payload = self._frame()
            if op == 0x8:
                raise RuntimeError("server closed the socket")
            if op in (0x9, 0xA):
                continue
            if op == 0x0:
                if pending_op is None:
                    continue
                pending += payload
                if not fin:
                    continue
                payload, pending, pending_op = pending, b"", None
            elif op == 0x1:
                if not fin:
                    pending, pending_op = payload, op
                    continue
            else:
                continue
            f = json.loads(payload)
            if want is None or want(f):
                return f
        raise RuntimeError("no matching frame within %.0fs" % within)

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


def join(ws, channel, who):
    """Join a channel through the front end and select its window."""
    ws.send({"type": "send", "text": "/join " + channel})
    try:
        ws.state(30.0, lambda f: channel in (f.get("windows") or []))
    except RuntimeError:
        print("  FAIL: %s never got a %s window" % (who, channel))
        return False
    ws.send({"type": "select", "active": channel})
    f = ws.state(15.0, lambda f: f.get("active") == channel)
    print("  %s: joined and selected %s" % (who, f.get("active")))
    return True


def main():
    stamp = int(time.time()) % 100000
    mark = "ws-live-%d" % stamp
    ok = True

    print("alice (%s):" % HOSTS["alice"])
    a = WS(HOSTS["alice"])
    f = a.state()
    print("  handshake 101; first frame: %d windows, %d bytes"
          % (len(f.get("windows") or []), len(f.get("render") or "")))
    if not join(a, CHANNEL, "alice"):
        return 1

    print("bob (%s):" % HOSTS["bob"])
    b = WS(HOSTS["bob"])
    b.state()
    # Bob must be in the channel BEFORE the line is sent, or his absence proves
    # nothing about delivery.
    if not join(b, CHANNEL, "bob"):
        return 1
    time.sleep(3)

    print("alice sends %r" % mark)
    a.send({"type": "send", "text": mark})

    # The measurement: bob's bouncer, a separate process with its own upstream
    # connection, can only have this line if it crossed the IRCd.
    try:
        b.state(30.0, lambda f: mark in (f.get("render") or ""))
        print("  bob's render now contains the line: yes")
    except RuntimeError:
        print("  FAIL: bob never saw the line, so it did not reach the IRCd")
        ok = False

    # And alice sees her own line without reloading: the live-render claim.
    try:
        a.state(15.0, lambda f: mark in (f.get("render") or ""))
        print("  alice's own render contains it without a reload: yes")
    except RuntimeError:
        print("  FAIL: alice's render never updated on its own")
        ok = False

    for ws in (a, b):
        ws.close()
    print("RESULT:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
