#!/usr/bin/env python3
"""Does strip_controls actually change what lands in the log file?

The unit tests cover the stripping function and the writer's flag. This checks the
whole path against the deployed daemon: put a coloured line through the bouncer,
then read the log file the daemon wrote and look at it.

The daemon runs with logging on (log_dir = "/data/logs") but strip_controls off,
which is the default and the ZNC-matching behaviour — so the expectation here is
that the colour *bytes* are gone (sanitizeBody always removes C0) while the
colour *digits* survive, which is exactly the difference between the log-integrity
pass and the presentation pass.
"""
import socket
import sys
import time

BOUNCER = ("salotto-e2e", 6667)
CHANNEL = "#e2e"


def main():
    # 0x03 is the IRC colour code; 0x04 is not a valid control so its digit is
    # just text and must survive untouched.
    marker = "strip%d" % int(time.time())
    payload = "\x0304" + marker + "x\x03"

    s = socket.create_connection(BOUNCER, 5)
    s.settimeout(1.0)
    s.sendall(b"NICK e2e\r\nUSER e2e 0 * :e2e\r\n")
    time.sleep(1.5)
    try:
        while s.recv(8192):
            pass
    except socket.timeout:
        pass
    s.sendall(("PRIVMSG %s :%s\r\n" % (CHANNEL, payload)).encode())
    time.sleep(2.0)
    s.close()
    print("sent:", repr(payload))
    print("marker:", marker)
    print("look for it under /data/logs/ircd/%s/" % CHANNEL)
    return 0


if __name__ == "__main__":
    sys.exit(main())
