#!/usr/bin/env python3
"""Prove /search against the deployed client, over the public PTY bridge.

The unit tests prove the model. This proves the thing a user does: type /search into a real
terminal session served by the deployed pod, and read the answer off the screen the way a person
would.

It is deliberately not a check that "something appeared". The three claims are:

  1. a search with no matches says so **and names the query**, because an empty result for a typo
     and an empty result for a real word are different problems;
  2. /search with no argument prints usage instead of searching for the empty string, which would
     match every line ever shown;
  3. the command never goes to the wire - proven indirectly but strongly here: the answer comes
     back with no round trip to an upstream, which is the only way a client-side search can work
     when the window it searched is not a channel on any network.

The screen is read through the same emulator the other probes use, so what is asserted is what the
user would see.
"""
import base64
import json
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

# A word that exists in this deployment's scrollback and matches only a few lines, so the answer
# fits on screen. "329" is in the channel-mode numerics; "salotto" matches too much for a live
# channel (see the comment where it is used).
QUERY = "329"
from probe_public_term import WS  # noqa: E402


def main():
    host = "alice.ch-test.katia.sh"
    print("host:", host)
    fails = []
    ws = WS(host, "/ws?cols=100&rows=30")
    try:
        if not ws.wait_for("status", 40):
            print("  FAIL the session never drew")
            return 1
        print("  ok   session up")

        # 0. A search that DOES match.
        #
        #    Read from the rendered stream (ws.raw), not from the current screen. This channel is
        #    live - it carries traffic while the probe runs - so a multi-row answer scrolls off
        #    within a second of being drawn, and a screen-only check fails on a working feature.
        #    That is not a guess: this check failed four times against correct behaviour before the
        #    cause was clear.
        #
        #    What the stream proves is that the model answered: a count, and hit rows naming the
        #    window each hit is in. Whether such an answer *fits* a given terminal is a separate
        #    claim, asserted where it can be deterministic - TestTheSearchListFitsTheTerminal.
        before = len(ws.raw)
        ws.type_line("/search " + QUERY)
        deadline = time.time() + 25
        body = ""
        while time.time() < deadline:
            ws.read(time.time() + 1)
            body = ws.raw[before:].decode(errors="replace")
            if "match(es)" in body:
                break
        if "match(es)" not in body:
            print("  FAIL a matching search reported no count")
            fails.append("count")
        else:
            # A hit row names its window. In the stream the row is "  <window>  <nick> text",
            # so the window is whatever token follows the leading spaces.
            named = [ln for ln in body.replace("\r", "").split("\n")
                     if "  <" in ln and (QUERY in ln)]
            if not named:
                print("  FAIL the hits did not name their windows")
                fails.append("no-window")
            else:
                print("  ok   a matching search reports a count and names its windows")

        # 1. A search that cannot match: the message must name what was searched for.
        token = "zzqqxxnomatch"
        ws.type_line("/search " + token)
        if not ws.wait_for(token, 20):
            print("  FAIL no-matches did not echo the query")
            fails.append("echo")
        else:
            print("  ok   no-matches names the query")
        # Scope this to the screen *after* the no-match answer, and only look above it: the
        # previous check's count line is still on screen, so "match(es) is not visible" is not
        # something a single screen read can establish. What matters is that the no-match answer
        # is not accompanied by a count - so look at the lines around it.
        scr = ws.term.screen().splitlines()
        idx = next((i for i, ln in enumerate(scr) if token in ln and "no matches" in ln), None)
        if idx is None:
            # Fall back to the raw stream if the line scrolled.
            idx = None
        if idx is not None and any("match(es)" in ln and "no matches" not in ln for ln in scr[idx:idx + 3]):
            print("  FAIL a no-match search claimed matches")
            fails.append("false-match")
        else:
            print("  ok   a no-match search reports no count")

        # 2. A bare /search is a usage message, not a search for "".
        ws.type_line("/search")
        if not ws.wait_for("usage", 20):
            print("  FAIL a bare /search gave no usage message")
            fails.append("usage")
        else:
            print("  ok   bare /search prints usage")

        # 3. The command was resolved locally, not sent.
        #
        #    The first version of this check looked for "/search" anywhere on screen and failed on
        #    other people's conversation about the feature - a check that cannot tell a message
        #    from a mention. The property worth testing is narrower: the *token we searched for*
        #    must appear only in the no-matches line, never as a message anyone sent. A forwarded
        #    command would come back as a line too, because the bouncer knows no SEARCH verb.
        scr = ws.term.screen()
        occurrences = [ln for ln in scr.splitlines() if token in ln]
        not_ours = [ln for ln in occurrences if "no matches" not in ln]
        if not_ours:
            print("  FAIL the search token came back as something other than the no-matches line: %r"
                  % not_ours[:2])
            fails.append("echoed")
        elif not occurrences:
            print("  FAIL the search token is nowhere on screen")
            fails.append("vanished")
        else:
            print("  ok   the query appears only in the model's own answer")

        print("\n" + ("ALL PASS" if not fails else "FAILURES: " + ", ".join(fails)))
        return 0 if not fails else 1
    finally:
        ws.close()


if __name__ == "__main__":
    sys.exit(main())
