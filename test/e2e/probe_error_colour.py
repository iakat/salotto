#!/usr/bin/env python3
"""Is a refusal drawn as a refusal, on the deployed front end?

The unit tests assert the model's kind and the renderer's escape code. This asserts the thing
neither can: that a real bouncer refusing a real command produces a red line in a real
terminal, on the public URL.

The command is a PRIVMSG to a nick that cannot exist. Every upstream answers that with 401, and
the bouncer passes it through, so nothing here depends on a feature of ours - only on the error
path being drawn.

Two things this probe had to learn, both from getting a confident wrong answer first:

1. The refusal lands in the *status* window, which also carries the whole registration replay.
   Typing there and looking at the screen reads the oldest replayed lines, not the answer, so
   the probe must drain the replay before it types.

2. Colour has to be read from the emulator's own cells. A frame contains escape codes whatever
   colour it draws, which is how three earlier colour checks passed while the screen was grey.
   That is why vt.py tracks a foreground colour per cell now.
"""

import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from probe_public_term import HOSTS, WS  # noqa: E402

ERROR_COLOUR = 203


def main():
    nick, host = next(iter(HOSTS.items()))
    print(f"=== {nick} ({host})")
    ws = WS(host, "/ws?cols=110&rows=34")
    try:
        ws.read(time.time() + 40)
        # Refusals are status-window lines, so go there first. A probe that types into
        # whatever window happens to be active reads a screen the answer is not on, and that
        # looks exactly like the feature being broken.
        ws.type_line("/go status")
        end = time.time() + 12
        while time.time() < end:
            ws.read(min(end, time.time() + 2))
            if "status" in ws.term.lines()[0]:
                break
        # Drain the registration replay: 001-005, the MOTD and the per-network bursts all
        # arrive in the status window over several seconds. Typing before that finishes puts
        # the answer behind an unread backlog.
        ws.read(time.time() + 20)

        # Type it and read back the input line before pressing Enter: if the "/" is being
        # dropped the line is chat, and a chat line gets no refusal at all, which reads as a
        # broken error path when the real fault is the probe's typing.
        for ch in "/msg salotto-probe-no-such-nick-zz9 hello":
            ws.key(ch)
        ws.read(time.time() + 2)
        typed = [l for l in ws.term.lines() if "> " in l]
        print("  input line:", typed[-1].strip()[:70] if typed else "MISSING")
        ws.key("\r")
        deadline = time.time() + 45
        while time.time() < deadline:
            ws.read(min(deadline, time.time() + 2))
            if any("401" in l for l in ws.term.lines()):
                break

        if not any("401" in l for l in ws.term.lines()):
            print("  (reading again after a further 20s, in case the upstream was slow)")
            ws.read(time.time() + 20)
        # Decisive: is the 401 anywhere in the byte stream this client received? If it is in
        # the bytes but not on the screen, the bouncer delivered it and the rendering is what
        # is wrong; if it is in neither, the bouncer never sent it to *this* client. Those are
        # two different bugs and guessing between them wastes a deployment.
        # Search for the distinctive part of the line, not the bare digits: "401" occurs
        # inside msgids, timestamps and 005 tokens, so a substring test on it answers yes for
        # reasons that have nothing to do with this probe. The nick cannot occur anywhere
        # else.
        # Not the nick: the client echoes what it typed, so the nick is in the stream before
        # anything reaches the network. The server's own words are the only ones that can
        # come from a refusal.
        in_bytes = b"No such nick" in ws.raw
        print(f"  the refusal is in the raw stream: {in_bytes}")
        row = next((i for i, l in enumerate(ws.term.lines()) if "401" in l), None)
        if row is None:
            if not in_bytes:
                print("  FAIL the refusal never reached this client at all")
            else:
                print("  FAIL the refusal arrived but is not drawn")
            print("  --- the whole screen:")
            for i, l in enumerate(ws.term.lines()):
                print(f"   {i}: {l[:96]}")
            for l in [x for x in ws.term.lines()[1:] if x.strip()][-8:]:
                print("    ", l[:100])
            ws.close()
            return 1

        print("  header now:", ws.term.lines()[0][:34])
        colours = ws.term.row_colours(row)
        print(f"  line: {ws.term.lines()[row][:96]}")
        print(f"  colours on that row: {sorted(c for c in colours if c is not None)}")
        if ERROR_COLOUR not in colours:
            print(f"  FAIL the refusal is not drawn in the error colour {ERROR_COLOUR}")
            ws.close()
            return 1
        print(f"  ok   the refusal is drawn in {ERROR_COLOUR}")
    finally:
        ws.close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
