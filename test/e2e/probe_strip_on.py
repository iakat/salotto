#!/usr/bin/env python3
"""Prove strip_controls works against the deployed daemon.

The earlier probe ran with the flag at its default (off) and showed the digits of
a colour code surviving, which is only half a proof: it demonstrates that
sanitizeBody eats the C0 bytes but says nothing about the module behaviour. This
one runs with strip_controls = true in the deployed ConfigMap, so the same line
must come back with the colour *code* gone entirely — digits included.

If both runs produce the same log line, the flag is dead and the row in QUEUE.md
is a lie.
"""
import os
import socket
import sys
import time


def line(host, port, nick, now):
    """Put a coloured PRIVMSG through the bouncer into #e2e."""
    s = socket.create_connection((host, port), timeout=10)
    f = s.makefile("rwb", buffering=0)
    f.write(("NICK %s\r\nUSER %s 0 * :probe\r\n" % (nick, nick)).encode())
    time.sleep(1.5)
    f.write(b"JOIN #e2e\r\n")
    time.sleep(1.0)
    # ZNC/salotto case: no password. \x0304 is red, \x03 ends it.
    f.write(("PRIVMSG #e2e :\x0304strip%d code\x03 tail\r\n" % now).encode())
    time.sleep(1.5)
    f.write(b"QUIT\r\n")
    s.close()
    return "strip%d code" % now


def main():
    host = os.environ.get("BOUNCER_HOST", "salotto-e2e")
    port = int(os.environ.get("BOUNCER_PORT", "6667"))
    nick = "stripprobe%d" % int(time.time() % 100000)
    stamp = int(time.time())
    body = line(host, port, nick, stamp)
    print("sent:", repr(body))
    print("marker:", "strip%d" % stamp)


if __name__ == "__main__":
    main()
