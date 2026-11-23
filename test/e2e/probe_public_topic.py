"""Does the topic show up, and does it follow the window, through the public URL?

The unit tests cover the parse and the frame. This covers the claim that matters to
a user: set a topic, see it in the browser, and see a DIFFERENT topic when you
switch windows - because a header showing the wrong channel's topic is confidently
wrong, which is worse than no header.

Run as a pod in hermes-generated-content. Needs probe_public_ws.py beside it.
"""
import sys
import time

sys.path.insert(0, "/x")
from probe_public_ws import WS, HOSTS


def join(ws, channel):
    ws.send({"type": "send", "text": "/join " + channel})
    ws.state(30.0, lambda f: channel in (f.get("windows") or []))
    ws.send({"type": "select", "active": channel})
    ws.state(15.0, lambda f: f.get("active") == channel)


def main():
    stamp = int(time.time()) % 100000
    one = "#topic-a-%d" % stamp
    two = "#topic-b-%d" % stamp
    ok = True

    a = WS(HOSTS["alice"])
    a.state()
    join(a, one)
    join(a, two)

    # Set a different topic in each, through the front end, so this is the path a
    # user takes rather than a synthetic injection.
    print("setting topics")
    a.send({"type": "select", "active": one})
    a.state(10.0, lambda f: f.get("active") == one)
    a.send({"type": "send", "text": "/topic " + one + " first-topic"})
    try:
        f = a.state(20.0, lambda f: f.get("active") == one and "first-topic" in (f.get("topic") or ""))
        print("  %s topic: %r" % (one, f.get("topic")))
    except RuntimeError:
        print("  FAIL: no topic arrived for", one)
        ok = False

    a.send({"type": "select", "active": two})
    a.state(10.0, lambda f: f.get("active") == two)
    a.send({"type": "send", "text": "/topic " + two + " second-topic"})
    try:
        f = a.state(20.0, lambda f: f.get("active") == two and "second-topic" in (f.get("topic") or ""))
        print("  %s topic: %r" % (two, f.get("topic")))
    except RuntimeError:
        print("  FAIL: no topic arrived for", two)
        ok = False

    # The header must follow the window. This is the part that is silently wrong in
    # a client that caches one topic per tab.
    a.send({"type": "select", "active": one})
    try:
        f = a.state(15.0, lambda f: f.get("active") == one and "first-topic" in (f.get("topic") or ""))
        print("switched back to %s, topic: %r" % (one, f.get("topic")))
        if "second-topic" in (f.get("topic") or ""):
            print("  FAIL: showing the other channel's topic")
            ok = False
    except RuntimeError:
        print("  FAIL: the topic did not follow the window switch")
        ok = False

    a.close()
    print("RESULT:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
