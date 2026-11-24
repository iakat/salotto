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

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
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

        # 0. A search that DOES match, run first while the window is still short enough for the
        #    answer to be on screen. "salotto" is certain to be present: it is in the topic, in the
        #    channel name's lines and in the replay.
        #
        #    This check lives here rather than at the end because the status window in this
        #    deployment accumulates several screens of replay, and a search's answer lands below
        #    the visible part - so a late check cannot tell "reported nothing" from "scrolled off",
        #    and an ambiguous check is worse than no check.
        ws.type_line("/search salotto")
        if not ws.wait_for("match(es)", 25):
            print("  FAIL a matching search reported no count")
            fails.append("count")
        else:
            # A hit row is "  <window>  <nick> text". The window name is what makes the
            # result a set of jumps rather than a wall of text.
            scr0 = ws.term.screen()
            hit_rows = [ln for ln in scr0.splitlines() if ln.strip().startswith("#")]
            if not hit_rows:
                print("  FAIL no hit named its window")
                fails.append("no-window")
            else:
                print("  ok   a matching search names the windows its hits are in (%d rows)"
                      % len(hit_rows))

        # 1. A search that cannot match: the message must name what was searched for.
        ws.type_line("/search zzqqxxnomatch")
        if not ws.wait_for("zzqqxxnomatch", 20):
            print("  FAIL no-matches did not echo the query")
            fails.append("echo")
        else:
            print("  ok   no-matches names the query")
        scr = ws.term.screen()
        if "match(es)" in scr:
            print("  FAIL a no-match search claimed matches")
            fails.append("false-match")

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
        token = "zzqqxxnomatch"
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
