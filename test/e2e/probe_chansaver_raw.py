#!/usr/bin/env python3
"""Why does a channel joined in-session not come back after a reconnect?

`case_chansaver`'s new positive control failed on BOTH bouncers, which means the
measurement is wrong somewhere, not that both implementations are broken. This
dumps every line either bouncer sends across a Disconnect/Connect so the cause is
visible instead of guessed at.

Run against one bouncer:  probe_chansaver_raw.py <host> <port> <nick> <dialect>
"""
import socket
import sys
import time


def main() -> int:
    host, port, nick, dialect = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4]
    chan = "#e2e-keep%d" % (int(time.time()) % 10000)
    s = socket.create_connection((host, port), timeout=15)
    s.settimeout(0.4)
    buf = b""

    def send(line: str) -> None:
        print(">>> %s" % line)
        s.sendall((line + "\r\n").encode())

    def drain(seconds: float) -> list[str]:
        nonlocal buf
        out, end = [], time.time() + seconds
        while time.time() < end:
            try:
                chunk = s.recv(65536)
                if not chunk:
                    break
                buf += chunk
            except socket.timeout:
                continue
            while b"\r\n" in buf:
                line, buf = buf.split(b"\r\n", 1)
                text = line.decode("utf-8", "replace")
                out.append(text)
                print("<<< %s" % text)
        return out

    if dialect == "znc":
        send("PASS e2e-client-password")
    send("NICK " + nick)
    send("USER %s 0 * :probe" % nick)
    drain(3.0)

    print("\n=== JOIN %s (in-session, so chansaver must save it)" % chan)
    send("JOIN " + chan)
    drain(2.5)

    print("\n=== Disconnect / Connect")
    if dialect == "znc":
        send("PRIVMSG *status :Disconnect")
    else:
        send("DISCONNECT")
    drain(2.0)
    if dialect == "znc":
        send("PRIVMSG *status :Connect")
    else:
        send("CONNECT")
    lines = drain(8.0)

    joined = [l for l in lines if " JOIN " in l.upper()]
    print("\n=== JOIN lines after reconnect: %r" % (joined,))
    print("=== expected channel: %s" % chan)

    print("\n=== the saved list, as the bouncer itself reports it")
    if dialect == "znc":
        send("PRIVMSG *status :ListChans")
    else:
        send("LISTCHANS")
    drain(2.5)

    print("\n=== clean up")
    send("PART " + chan)
    drain(1.0)
    send("QUIT")
    s.close()
    return 0 if joined else 1


if __name__ == "__main__":
    sys.exit(main())
