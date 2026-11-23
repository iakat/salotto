"""Is the nicklist real through the public front ends?

A unit test proves the parse. This proves the thing a user sees: alice's browser
shows a member list, and when bob joins the channel his name appears in it without
anyone reloading.

The honest part is the second half. A nicklist that is correct at join time and
never updates looks perfectly fine on first load, which is the failure mode of
every "maintained" list. So bob arrives AFTER alice's list is already rendered.

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


def names(frame):
    return [n.get("name") for n in (frame.get("nicks") or [])]


def main():
    ok = True

    a = WS(HOSTS["alice"])
    a.state()
    join(a, CHANNEL)
    # Alice asks for the names; the IRCd answers with 353/366, which the bouncer
    # turns into a Nicklist on her socket.
    a.send({"type": "send", "text": "/names " + CHANNEL})
    try:
        f = a.state(30.0, lambda f: f.get("active") == CHANNEL and f.get("nicks"))
        print("alice's nicklist:", names(f))
    except RuntimeError:
        print("FAIL: alice never received a nicklist for", CHANNEL)
        return 1
    if "alice" not in names(f):
        print("FAIL: alice's own nick is not in the nicklist:", names(f))
        ok = False
    before = set(names(f))

    # Now bob joins, and alice's list must grow by itself.
    print("bob joins %s" % CHANNEL)
    b = WS(HOSTS["bob"])
    b.state()
    join(b, CHANNEL)
    time.sleep(3)

    try:
        f2 = a.state(30.0, lambda f: f.get("active") == CHANNEL and "bob" in names(f))
        print("alice's nicklist now:", names(f2))
        added = set(names(f2)) - before
        if "bob" not in added:
            print("FAIL: bob appeared without being new:", added)
            ok = False
        else:
            print("  bob appeared with nobody reloading:", "yes")
    except RuntimeError:
        print("FAIL: bob joined but alice's nicklist never updated")
        print("  (a nicklist correct only at join time looks fine on first load)")
        ok = False

    # And a departure has to remove him, or the list is authoritative-looking and
    # stale, which is worse than absent.
    print("bob parts %s" % CHANNEL)
    b.send({"type": "send", "text": "/part " + CHANNEL})
    try:
        f3 = a.state(30.0, lambda f: f.get("active") == CHANNEL and "bob" not in names(f))
        print("alice's nicklist after part:", names(f3))
    except RuntimeError:
        print("FAIL: bob parted and alice still lists him")
        ok = False

    a.close()
    b.close()
    print("RESULT:", "PASS" if ok else "FAIL")
    return 0 if ok else 1


if __name__ == "__main__":
    sys.exit(main())
