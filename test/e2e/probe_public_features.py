"""Do mentions and search work through the public front ends?

Both features are client-side consequences of server state, so the only honest test
drives them the way a user does: over the socket, through the gateway.

  1. alice and bob both join a channel.
  2. bob says alice's name. alice's frame must carry the mention, and if she is
     looking at another window it must be badged as a mention, not as unread.
  3. alice searches for a phrase only bob's line contains. The results must name
     the right window - which is the part a browser-only search could not do,
     since it would only see what has been rendered into the page.

Run as a pod in hermes-generated-content. Needs probe_public_ws.py beside it.
"""
import sys
import time

sys.path.insert(0, "/x")
from probe_public_ws import WS, HOSTS, CHANNEL


def join(ws, channel):
    ws.send({"type": "send", "text": "/join " + channel})
    ws.state(30.0, lambda f: channel in (f.get("windows") or []))
    ws.send({"type": "select", "active": channel})
    ws.state(15.0, lambda f: f.get("active") == channel)


def main():
    stamp = int(time.time()) % 100000
    needle = "pineapple-%d" % stamp
    ok = True

    a = WS(HOSTS["alice"])
    a.state()
    join(a, CHANNEL)
    b = WS(HOSTS["bob"])
    b.state()
    join(b, CHANNEL)
    time.sleep(3)

    # 1. A mention while alice is looking elsewhere. She must not be looking at
    #    the channel, or the mention is just a highlighted line and the sidebar
    #    claim goes untested.
    a.send({"type": "select", "active": "status"})
    a.state(10.0, lambda f: f.get("active") == "status")

    print("bob mentions alice in %s" % CHANNEL)
    b.send({"type": "send", "text": "alice: " + needle})

    try:
        f = a.state(30.0, lambda f: (f.get("mentions") or {}).get(CHANNEL))
        print("  alice's sidebar badges it as a mention: yes")
    except RuntimeError:
        print("  FAIL: alice's frame never marked %s as a mention" % CHANNEL)
        ok = False
    else:
        if int((f.get("unread") or {}).get(CHANNEL, 0)) < 1:
            print("  FAIL: the mention did not count as unread too")
            ok = False

    # 2. Search, from alice, for text only bob's line contains.
    print("alice searches for %r" % needle)
    a.send({"type": "search", "query": needle})
    try:
        r = a.frame(20.0, lambda f: f.get("type") == "results" and f.get("query") == needle)
    except RuntimeError:
        print("  FAIL: no results frame came back")
        return 1
    hits = r.get("results") or []
    print("  hits: %d" % len(hits))
    if not hits:
        print("  FAIL: search found nothing although bob's line is in the buffer")
        ok = False
    else:
        windows = sorted({h.get("window") for h in hits})
        print("  windows:", windows)
        if CHANNEL not in windows:
            print("  FAIL: the hit is not attributed to %s" % CHANNEL)
            ok = False
        if not any(needle in (h.get("text") or "") for h in hits):
            print("  FAIL: the hit text does not contain the query")
            ok = False

    a.close()
    b.close()
    print("RESULT:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
