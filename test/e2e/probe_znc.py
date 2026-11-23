import socket, sys, time

host, port = sys.argv[1], int(sys.argv[2])
nick, pw = sys.argv[3], sys.argv[4]
done = 0  # unused

def collect(s, secs):
    buf = b""
    end = time.time() + secs
    s.settimeout(0.3)
    while time.time() < end:
        try:
            c = s.recv(65536)
            if not c:
                break
            buf += c
        except socket.timeout:
            pass
    return [l.decode("utf-8", "replace") for l in buf.split(b"\r\n") if l]

def show(label, lines):
    print("=== %s" % label)
    for l in lines:
        print("  |", l)

# 1. handshake only, no CAP REQ (so no echo-message)
s = socket.create_connection((host, port), timeout=10)
for l in ["PASS " + pw, "CAP LS 302", "NICK " + nick, "USER %s 0 * :x" % nick, "CAP END"]:
    s.sendall(l.encode() + b"\r\n")
show("handshake", collect(s, 4))

s.sendall(("JOIN #e2e").encode() + b"\r\n")
show("after JOIN", collect(s, 3))

s.sendall(("PRIVMSG #e2e :echo-probe-1").encode() + b"\r\n")
show("after PRIVMSG (no echo-message)", collect(s, 3))

# 2. Registers a context and asks for echo-message explicitly.
s.sendall(("CAP REQ :echo-message").encode() + b"\r\n")
show("cap req echo-message", collect(s, 2))
s.sendall(("PRIVMSG #e2e :echo-probe-2").encode() + b"\r\n")
show("after PRIVMSG (with echo-message)", collect(s, 3))

# 3. Playback paths.
s.sendall(("DETACH #e2e").encode() + b"\r\n")
show("detach", collect(s, 2))
s.sendall(("PRIVMSG #e2e :while-detached").encode() + b"\r\n")
show("msg while detached", collect(s, 2))
s.sendall(("PLAYBUFFER #e2e").encode() + b"\r\n")
show("playbuffer", collect(s, 3))

s.sendall(b"QUIT :done\r\n")
s.close()
