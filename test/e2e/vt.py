"""A small VT100 emulator, because reading a terminal is not reading a text file.

A program that owns a terminal does not append lines: it moves the cursor and paints.
bubbletea redraws the frame in place with cursor addressing, and after the first frame it
sends only the cells that changed. So "the last chunk of bytes since the last full draw" is
not the screen - it is a patch, and a probe that calls it the screen reports a live UI as
blank.

This emulator keeps a grid and applies what the program wrote: cursor movement, erase,
scrolling, wrapping, and SGR (ignored - colours are not what a feature check measures). It
is not a complete xterm; it covers what bubbletea and lipgloss emit, which is CUP, EL, ED,
SGR, DEC private modes, and plain text.

The point is to measure the product the way a person sees it, on the emulator an actual
browser runs, without a browser.
"""

import codecs

class Term:
    def __init__(self, cols=100, rows=30):
        self.cols, self.rows = cols, rows
        self.grid = [[" "] * cols for _ in range(rows)]
        # fg is the foreground colour of each cell, in parallel with grid. It exists because
        # this emulator used to say "colour is not a feature this measures", and that is how
        # three separate colour checks passed while the screen was grey: a frame contains
        # escape codes whatever colour it draws. A cell that was never written keeps None.
        self.fg = [[None] * cols for _ in range(rows)]
        self.cur_fg = None
        self.x = self.y = 0
        self.saved = (0, 0)
        self.buf = b""
        # A chunk boundary is not a character boundary: bubbletea writes the box drawing
        # glyphs as three-byte UTF-8, and a pty read can split one. Decoding each chunk on
        # its own turns half a glyph into U+FFFD, and a screen check then reports a frame it
        # cannot read. The incremental decoder carries the partial sequence over.
        self.dec = codecs.getincrementaldecoder("utf-8")("replace")

    # -- output -----------------------------------------------------------------
    def feed(self, data):
        """Apply bytes to the grid.

        The one thing that matters here is that a partial escape sequence is not text. A
        pty read is an arbitrary slice of the stream, so "\x1b[38;5;63m" can arrive as
        "\x1b[38" in one chunk and ";5;63m" in the next. An earlier version of this file
        treated a non-matching ESC as a two-byte escape and printed the rest of the
        sequence on screen ("38;5;63m|"), which both corrupted the grid and hid the real
        content. Incomplete sequences are kept and retried on the next chunk.
        """
        self.buf += data
        while self.buf:
            b = self.buf
            if b[0:1] == b"\x1b":
                if len(b) < 2:
                    return
                kind = b[1:2]
                if kind == b"[":
                    i = 2
                    while i < len(b) and not (0x40 <= b[i] <= 0x7E):
                        i += 1
                    if i >= len(b):
                        return  # incomplete: wait for the final byte
                    self.csi(b[2:i].decode("latin-1"), chr(b[i]))
                    self.buf = b[i + 1:]
                    continue
                if kind == b"]":
                    end = b.find(b"\x07")
                    alt = b.find(b"\x1b\\")
                    if alt >= 0 and (end < 0 or alt < end):
                        self.buf = b[alt + 2:]
                        continue
                    if end >= 0:
                        self.buf = b[end + 1:]
                        continue
                    return  # OSC still open
                self.esc(kind.decode("latin-1"))
                self.buf = b[2:]
                continue
            n = b.find(b"\x1b")
            chunk = b if n < 0 else b[:n]
            self.put(chunk)
            self.buf = b"" if n < 0 else b[n:]

    def put(self, chunk):
        for ch in self.dec.decode(chunk):
            if ch == "\r":
                self.x = 0
            elif ch == "\n":
                self.newline()
            elif ch == "\b":
                self.x = max(0, self.x - 1)
            elif ch == "\t":
                self.x = min(self.cols - 1, (self.x // 8 + 1) * 8)
            elif ch == "\x07":
                pass
            elif ord(ch) < 32:
                pass
            else:
                if self.x >= self.cols:
                    self.x = 0
                    self.newline()
                self.grid[self.y][self.x] = ch
                self.fg[self.y][self.x] = self.cur_fg
                self.x += 1

    def newline(self):
        if self.y + 1 >= self.rows:
            self.grid.pop(0)
            self.grid.append([" "] * self.cols)
            self.fg.pop(0)
            self.fg.append([None] * self.cols)
        else:
            self.y += 1

    def csi(self, params, final):
        if params.startswith("?"):
            # DEC private modes. The only one with visible consequences is the alternate
            # screen: entering it clears it, leaving it restores the normal screen. Cursor
            # visibility and mouse reporting do not change any cell.
            modes = {int(p) for p in params[1:].split(";") if p.isdigit()}
            if modes & {1049, 1047, 47}:
                self.grid = [[" "] * self.cols for _ in range(self.rows)]
                self.fg = [[None] * self.cols for _ in range(self.rows)]
                self.x = self.y = 0
            return
        args = [int(p) if p else 0 for p in params.replace(":", ";").split(";")] if params else []
        n = args[0] if args else 0
        if final in ("H", "f"):
            self.y = min(self.rows - 1, max(0, (args[0] if args else 1) - 1))
            self.x = min(self.cols - 1, max(0, (args[1] if len(args) > 1 else 1) - 1))
        elif final == "A":
            self.y = max(0, self.y - max(1, n))
        elif final == "B":
            self.y = min(self.rows - 1, self.y + max(1, n))
        elif final == "C":
            self.x = min(self.cols - 1, self.x + max(1, n))
        elif final == "D":
            self.x = max(0, self.x - max(1, n))
        elif final == "G":
            self.x = min(self.cols - 1, max(0, max(1, n) - 1))
        elif final == "d":
            self.y = min(self.rows - 1, max(0, max(1, n) - 1))
        elif final == "K":
            row = self.grid[self.y]
            if n == 0:
                for i in range(self.x, self.cols):
                    row[i] = " "
            elif n == 1:
                for i in range(0, self.x + 1):
                    row[i] = " "
            else:
                self.grid[self.y] = [" "] * self.cols
        elif final == "J":
            if n == 0:
                for i in range(self.x, self.cols):
                    self.grid[self.y][i] = " "
                for r in range(self.y + 1, self.rows):
                    self.grid[r] = [" "] * self.cols
            elif n == 1:
                for r in range(0, self.y):
                    self.grid[r] = [" "] * self.cols
                for i in range(0, self.x + 1):
                    self.grid[self.y][i] = " "
            else:
                self.grid = [[" "] * self.cols for _ in range(self.rows)]
                self.x = self.y = 0
        elif final == "m":
            self.sgr(args)
        elif final == "L":
            for _ in range(max(1, n)):
                self.grid.insert(self.y, [" "] * self.cols)
                self.grid.pop()
        elif final == "M":
            for _ in range(max(1, n)):
                self.grid.pop(self.y)
                self.grid.append([" "] * self.cols)
        elif final == "X":
            row = self.grid[self.y]
            for i in range(self.x, min(self.cols, self.x + max(1, n))):
                row[i] = " "

    def sgr(self, args):
        """Apply a Select Graphic Rendition. Only foreground colour is tracked: it is the one
        attribute the UI uses to mean something (an error, a mention, a nick), and the rest
        would be a graphics engine nobody asked for."""
        args = args or [0]
        i = 0
        while i < len(args):
            a = args[i]
            if a == 0 or a == 39:
                self.cur_fg = None
            elif a == 38 and i + 2 < len(args) and args[i + 1] == 5:
                self.cur_fg = args[i + 2]
                i += 2
            elif 30 <= a <= 37:
                self.cur_fg = a - 30
            elif 90 <= a <= 97:
                self.cur_fg = a - 90 + 8
            i += 1

    def row_colours(self, row):
        """The distinct foreground colours on a screen row, ignoring blank cells."""
        out = set()
        for i, ch in enumerate(self.grid[row]):
            if ch.strip():
                out.add(self.fg[row][i])
        return out

    def esc(self, ch):
        if ch == "7":
            self.saved = (self.x, self.y)
        elif ch == "8":
            self.x, self.y = self.saved
        elif ch == "M":
            if self.y == 0:
                self.grid.insert(0, [" "] * self.cols)
                self.grid.pop()
            else:
                self.y -= 1

    # -- reading ----------------------------------------------------------------
    def lines(self):
        return ["".join(r).rstrip() for r in self.grid]

    def screen(self):
        return "\n".join(self.lines())
