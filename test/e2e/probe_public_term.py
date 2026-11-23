#!/usr/bin/env python3
"""Drive the deployed web front end the way a browser does: open the websocket, read the
bytes, type into the terminal, and assert the model answers.

This is the public proof for the PTY bridge. It runs against the edge Gateway over HTTPS,
not against a local test server, so what it proves is that the thing users can reach is a
working terminal: escape sequences arrive, the model's frame is drawn, a keystroke reaches
the input line, and a resize redraws at the new width.

A websocket handshake and terminal output are both binary, so this hand-rolls the client
rather than pulling a dependency the deployment does not have.
"""

import base64
import json
import os
import re
import socket
import ssl
import struct
import sys
import time

ANSI = re.compile(rb"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)|\x1b[()][A-Z0-9]")


def type_line(ws, text):
    """Type a line a key at a time, spaces included, then Enter.

    A space is its own key on a terminal. Sending the whole line as one input event works
    for plain characters and silently loses every space, which is how "/join #chan" became
    "/join#chan" with no error anywhere.
    """
    for ch in text:
        ws.send(json.dumps({"type": "input", "data": base64.b64encode(ch.encode()).decode()}))
    ws.send(json.dumps({"type": "input", "data": base64.b64encode(b"\r").decode()}))


def screen(chunks):
    """The newest complete screen: the last full draw, escape codes removed.

    A terminal stream is a sequence of redraws, not lines appended to a file. Accumulating
    every byte from every redraw makes each message appear once per repaint, which reads
    as "messages are duplicated" when the terminal is fine. Taking the last draw and
    stripping the sequences gives what a person sees.
    """
    text = b"".join(chunks)
    # Each full render starts by moving the cursor home.
    parts = text.split(b"\x1b[H")
    last = parts[-1] if parts else text
    out = ANSI.sub(b"", last).decode("utf-8", "replace")
    return out.replace("\r\n", "\n")


def visible(line):
    """A line as a terminal would draw it: escape sequences are not characters.

    This matters more than it looks. Counting the raw bytes of a border makes a 40-column
    frame measure 91 'characters', which reads as "the resize did nothing" when in fact the
    model resized perfectly - the colour codes are just longer than the border.
    """
    return ANSI.sub(b"", line).decode("utf-8", "replace")


HOSTS = {
    "alice": "alice.ch-test.katia.sh",
    "bob": "bob.ch-test.katia.sh",
}


class WS:
    """The smallest websocket client that can carry a terminal: one frame at a time."""

    def __init__(self, host, path, timeout=30):
        self.host = host
        self.timeout = timeout
        ctx = ssl.create_default_context()
        self.sock = socket.create_connection((host, 443), timeout=timeout)
        self.sock = ctx.wrap_socket(self.sock, server_hostname=host)
        key = base64.b64encode(os.urandom(16)).decode()
        req = (
            f"GET {path} HTTP/1.1\r\n"
            f"Host: {host}\r\n"
            "Upgrade: websocket\r\n"
            "Connection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\n"
            "Sec-WebSocket-Version: 13\r\n"
            f"Origin: https://{host}\r\n"
            "\r\n"
        )
        self.sock.sendall(req.encode())
        resp = b""
        while b"\r\n\r\n" not in resp:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise RuntimeError("handshake closed")
            resp += chunk
        head = resp.split(b"\r\n\r\n")[0].decode("latin1")
        if "101" not in head.split("\r\n")[0]:
            raise RuntimeError("handshake refused: " + head.split("\r\n")[0])
        self.buf = b""

    def send(self, text):
        payload = text.encode()
        mask = os.urandom(4)
        masked = bytes(b ^ mask[i % 4] for i, b in enumerate(payload))
        n = len(payload)
        if n < 126:
            header = struct.pack("!BB", 0x81, 0x80 | n)
        elif n < 65536:
            header = struct.pack("!BBH", 0x81, 0x80 | 126, n)
        else:
            header = struct.pack("!BBQ", 0x81, 0x80 | 127, n)
        self.sock.sendall(header + mask + masked)

    def _read_exact(self, n):
        while len(self.buf) < n:
            chunk = self.sock.recv(65536)
            if not chunk:
                raise RuntimeError("closed")
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def recv(self):
        b1, b2 = self._read_exact(2)
        opcode = b1 & 0x0F
        n = b2 & 0x7F
        if n == 126:
            n = struct.unpack("!H", self._read_exact(2))[0]
        elif n == 127:
            n = struct.unpack("!Q", self._read_exact(8))[0]
        payload = self._read_exact(n)
        if opcode == 0x8:
            raise RuntimeError("server closed the session")
        if opcode == 0x9:
            return None  # ping; ignored
        return payload

    def text(self, deadline):
        """Accumulate output frames until the deadline; returns the bytes seen."""
        got = b""
        while time.time() < deadline:
            try:
                payload = self.recv()
            except (RuntimeError, socket.timeout):
                break
            if not payload:
                continue
            try:
                msg = json.loads(payload)
            except ValueError:
                continue
            if msg.get("type") == "output" and msg.get("data"):
                got += base64.b64decode(msg["data"])
                if b"\xe2\x95\xb0" in got:  # the frame's bottom-left corner
                    break
        return got

    def read(self, deadline):
        """Collect output until the deadline, as a list of chunks in arrival order.

        Chunks rather than one blob, because screen() needs to find the last full draw and
        a blob has already lost where the draws began. Terminal output is a stream of
        repaints; concatenating them makes each message appear once per repaint.
        """
        chunks = []
        # A short socket timeout so an idle gap does not end the read: the terminal goes
        # quiet between repaints, and stopping at the first quiet moment returns an empty
        # screen that looks like a broken renderer.
        self.sock.settimeout(1.0)
        while time.time() < deadline:
            try:
                payload = self.recv()
            except socket.timeout:
                continue
            except RuntimeError:
                break
            if not payload:
                continue
            try:
                msg = json.loads(payload)
            except ValueError:
                continue
            if msg.get("type") == "output" and msg.get("data"):
                chunks.append(base64.b64decode(msg["data"]))
        return chunks

    def close(self):
        try:
            self.sock.close()
        except OSError:
            pass


def main():
    fails = []

    for nick, host in HOSTS.items():
        print(f"=== {nick} ({host})")
        ws = WS(host, "/ws?cols=100&rows=30")
        try:
            got = ws.text(time.time() + 45)
        except Exception as exc:  # noqa: BLE001 - reported, not raised
            print(f"  FAIL handshake/render: {exc}")
            fails.append(nick)
            ws.close()
            continue

        # 1. The child thinks it is on a terminal: alt screen and mouse reporting.
        if b"\x1b[?1049h" not in got:
            print("  FAIL no alt-screen sequence: the child is not on a terminal")
            fails.append(nick)
        else:
            print("  ok   alt screen")

        # 2. The model drew the frame. This is the "chrome is gone" bug: the page
        #    showed something the model had not drawn.
        if "╰".encode() not in got:
            print("  FAIL the model's frame is not in the output")
            fails.append(nick)
        else:
            print("  ok   model frame")

        # 3. A keystroke reaches the model's own input line.
        ws.send(json.dumps({"type": "input", "data": base64.b64encode(b"/help").decode()}))
        typed = ws.text(time.time() + 20)
        if b"/help" not in typed:
            print("  FAIL the keystroke never reached the model's input line")
            fails.append(nick)
        else:
            print("  ok   keystroke reached the model")

        # 4. A resize reaches the model: the border it redraws is the new width.
        #
        # Wait for a NARROW border rather than reading whatever is queued: output from
        # before the resize is still in flight, and the first frame a naive reader sees is
        # the old one. That is the difference between "the resize worked" and "the resize
        # worked and the probe looked at the previous screen".
        ws.send(json.dumps({"type": "resize", "cols": 40, "rows": 12}))
        width = None
        deadline = time.time() + 25
        while time.time() < deadline and width is None:
            chunk = ws.text(time.time() + 5)
            for raw in chunk.split(b"\r\n"):
                if "\u256d".encode() not in raw:
                    continue
                # Visible width, not bytes and not raw characters: the line carries
                # colour codes, and measuring those makes every frame look wide.
                w = len(visible(raw).rstrip())
                if w <= 45:
                    width = w
                    break
        if width is None:
            print("  FAIL no narrow redraw after the resize: the model ignored it")
            fails.append(nick)
        else:
            print(f"  ok   resize reached the model ({width} chars in the top border)")

        # 5. A real conversation: join a channel and say something.
        #
        # This is what a person does first, and it is where both reported bugs lived: an
        # unregistered client showed no windows at all, and a registered one drew every
        # message twice. Assert on the SCREEN, not on accumulated output.
        chan = "#salotto-probe"
        type_line(ws, "/join " + chan)
        joined = ws.read(time.time() + 15)
        scr = screen(joined)
        if chan not in scr:
            print(f"  FAIL {chan} never appeared in the window list")
            fails.append(nick)
        else:
            print("  ok   join opened a window")

        body = f"probe {nick} message"
        type_line(ws, body)
        said = ws.read(time.time() + 15)
        scr = screen(said)
        n = scr.count(body)
        if n == 0:
            print("  FAIL our own message never appeared")
            fails.append(nick)
        elif n > 1:
            print(f"  FAIL our own message is on screen {n} times: it is echoed locally and by the bouncer")
            fails.append(nick)
        else:
            print("  ok   our own message appears exactly once")

        ws.close()

    if fails:
        print("FAILED:", ", ".join(sorted(set(fails))))
        return 1
    print("PASS: both deployments are real terminals over the public edge")
    return 0


if __name__ == "__main__":
    sys.exit(main())
