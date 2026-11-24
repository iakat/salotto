#!/usr/bin/env python3
"""Drive the deployed web front end the way a browser does: open the websocket, be a
terminal, and assert on the screen a person would see.

This is the public proof for the PTY bridge. It runs against the edge Gateway over HTTPS,
not against a local test server, so what it proves is that the thing users can reach is a
working terminal.

Three things made earlier versions of this probe lie, and each produced a confident wrong
answer, so each is worth stating:

1. A program that owns a terminal does not append lines, it paints. bubbletea draws the
   frame with cursor addressing and then sends only the cells that changed, so "the bytes
   since the last full draw" is a patch and not a screen. Reading it as text reports a
   working UI as blank. Hence vt.py: an emulator that keeps a grid.

2. The child asks the terminal questions before it draws anything - the background colour
   (OSC 11) and the cursor position (DSR 6n) - and waits for the answers. xterm.js answers
   them; a script that only reads gets the mode-setting sequences and then silence, and
   that silence looks exactly like a broken front end. So this probe answers, because a
   terminal answers.

3. A space is its own key on a terminal. Sending a whole line as one input event works for
   letters and silently drops every space, which is how "/join #chan" once became
   "/join#chan" with no error anywhere.
"""

import base64
import json
import os
import socket
import ssl
import struct
import sys
import time

from vt import Term

HOSTS = {
    "alice": "alice.ch-test.katia.sh",
    "bob": "bob.ch-test.katia.sh",
}

# What a terminal says when asked. The UI is designed for a dark background, and the cursor
# is where bubbletea expects it to be: at home.
ANSWER_BG = b"\x1b]11;rgb:0000/0000/0000\x1b\\"
ANSWER_DSR = b"\x1b[1;1R"


class WS:
    """The smallest websocket client that can carry a terminal, plus a screen."""

    def __init__(self, host, path, timeout=30):
        self.host = host
        self.raw = b""
        cols, rows = 100, 30
        if "cols=" in path and "rows=" in path:
            q = path.split("?", 1)[1]
            for part in q.split("&"):
                k, _, v = part.partition("=")
                if k == "cols" and v.isdigit():
                    cols = int(v)
                if k == "rows" and v.isdigit():
                    rows = int(v)
        self.term = Term(cols, rows)

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
        head, _, rest = resp.partition(b"\r\n\r\n")
        first = head.split(b"\r\n")[0].decode("latin1")
        if "101" not in first:
            raise RuntimeError("handshake refused: " + first)
        # The replies to the terminal's own questions (OSC 11, DSR) go out during the
        # handshake, and the child's first frame can arrive in the same TCP segment. Keeping
        # only the header and starting empty throws those bytes away, and a frame with its
        # first cells missing draws a screen with no top border - which reads as "the model
        # never drew a frame" on roughly one run in three.
        self.buf = rest

    def send(self, text):
        payload = text.encode() if isinstance(text, str) else text
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
        """One message, reassembled from however many frames carry it.

        A websocket message may be split across frames (first frame with the text opcode,
        then continuation frames with opcode 0). The edge proxy in front of the deployment
        does split the big ones: the first frame of a frame-sized render arrives whole and
        the rest arrive as continuations. A reader that decodes each frame on its own gets
        the first fragment, fails to parse it as JSON, drops it, and reports a screen missing
        its top rows - which is exactly what "the model's frame is not on screen" was.
        """
        payload = b""
        first_op = None
        while True:
            b1, b2 = self._read_exact(2)
            fin = bool(b1 & 0x80)
            opcode = b1 & 0x0F
            n = b2 & 0x7F
            if n == 126:
                n = struct.unpack("!H", self._read_exact(2))[0]
            elif n == 127:
                n = struct.unpack("!Q", self._read_exact(8))[0]
            chunk = self._read_exact(n)
            if opcode == 0x8:
                raise RuntimeError("server closed the session")
            if opcode == 0x9:  # ping
                if fin and not payload:
                    return None
                continue
            if opcode == 0xA:  # pong
                if fin and not payload:
                    return None
                continue
            if opcode == 0x0:  # continuation
                payload += chunk
            else:
                if first_op is None:
                    first_op = opcode
                payload = chunk if not payload else payload + chunk
            if fin:
                return payload

    def key(self, ch, pause=0.05):
        self.send(json.dumps({"type": "input", "data": base64.b64encode(ch.encode()).decode()}))
        time.sleep(pause)

    def type_line(self, text):
        for ch in text:
            self.key(ch)
        self.key("\r")

    def answer(self, data):
        """Be a terminal: reply to the questions the program asks."""
        if b"\x1b]11;?" in data:
            self.send(json.dumps({"type": "input", "data": base64.b64encode(ANSWER_BG).decode()}))
        if b"\x1b[6n" in data:
            self.send(json.dumps({"type": "input", "data": base64.b64encode(ANSWER_DSR).decode()}))

    def read(self, deadline):
        """Read until the deadline, feeding the emulator. Returns the bytes seen."""
        self.sock.settimeout(1.0)
        got = b""
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
            if msg.get("type") != "output" or not msg.get("data"):
                continue
            data = base64.b64decode(msg["data"])
            got += data
            self.raw += data
            self.answer(data)
            self.term.feed(data)
        return got

    def wait_for(self, needle, within):
        """Read until needle is on the screen, or give up. True if it appeared."""
        end = time.time() + within
        while time.time() < end:
            self.read(min(end, time.time() + 2))
            if needle in self.term.screen():
                return True
        return False

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
            ws.read(time.time() + 45)
        except Exception as exc:  # noqa: BLE001 - reported, not raised
            print(f"  FAIL handshake/render: {exc}")
            fails.append(nick)
            ws.close()
            continue

        # 1. The child thinks it is on a terminal: alt screen and mouse reporting.
        if b"\x1b[?1049h" not in ws.raw:
            print("  FAIL no alt-screen sequence: the child is not on a terminal")
            fails.append(nick)
        else:
            print("  ok   alt screen")

        # 2. The model drew the frame, and it is on the screen a person reads. This is the
        #    "chrome is gone" bug: the page showed what the model had not drawn.
        scr = ws.term.screen()
        if "╰" not in scr or "╭" not in scr:
            print("  FAIL the model's frame is not on screen")
            fails.append(nick)
        elif "status" not in scr:
            print("  FAIL no window list on a fresh session")
            fails.append(nick)
        else:
            print("  ok   model frame with the window list")

        # 3. A keystroke reaches the model's own input line.
        for ch in "/help":
            ws.key(ch)
        if not ws.wait_for("/help", 20):
            print("  FAIL the keystroke never reached the model's input line")
            fails.append(nick)
        else:
            print("  ok   keystroke reached the model")

        # Clear the line before the next step, or the join is appended to it.
        for _ in range(5):
            ws.key("\x7f", 0.02)

        # 4. A resize reaches the model: the emulated screen is re-laid out at the new
        #    width, not just told about it.
        ws.send(json.dumps({"type": "resize", "cols": 40, "rows": 12}))
        ws.term.cols, ws.term.rows = 40, 12
        ws.term.grid = [[" "] * 40 for _ in range(12)]
        ws.read(time.time() + 8)
        borders = [l for l in ws.term.screen().split("\n") if l.startswith("╭")]
        if not borders or len(borders[-1]) > 40:
            print(f"  FAIL no narrow redraw after the resize: {borders[:1]}")
            fails.append(nick)
        else:
            print(f"  ok   resize reached the model ({len(borders[-1])} columns)")
        ws.send(json.dumps({"type": "resize", "cols": 100, "rows": 30}))
        ws.term.cols, ws.term.rows = 100, 30
        ws.term.grid = [[" "] * 100 for _ in range(30)]
        ws.read(time.time() + 8)

        # 5. A real conversation: join a channel and say something.
        #
        # This is the first thing a person does, and it is where both reported bugs lived:
        # an unregistered client showed no windows at all, and a registered one drew every
        # message twice. Assert on the screen, not on accumulated output.
        chan = "#salotto-probe"
        ws.type_line("/join " + chan)
        if not ws.wait_for(chan, 25):
            print(f"  FAIL {chan} never appeared in the window list")
            fails.append(nick)
        else:
            print("  ok   join opened a window")

        body = f"probe {nick} says hi"
        ws.type_line(body)
        ws.read(time.time() + 12)
        n = ws.term.screen().count(body)
        if n == 0:
            print("  FAIL our own message never appeared")
            fails.append(nick)
        elif n > 1:
            print(f"  FAIL our own message is on screen {n} times")
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
