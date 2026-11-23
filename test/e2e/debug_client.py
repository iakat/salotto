import socket, time, sys

host, port = sys.argv[1], int(sys.argv[2])
nick = sys.argv[3]
pw = sys.argv[4] if len(sys.argv) > 4 else None

s = socket.create_connection((host, port), timeout=10)
s.settimeout(0.5)
buf = b""

def drain(t=1.0):
    global buf
    end = time.time() + t
    while time.time() < end:
        try:
            c = s.recv(65536)
            if not c:
                print("<< EOF from server")
                return
            buf += c
        except socket.timeout:
            pass
    while b"\n" in buf:
        line, buf = buf.split(b"\n", 1)
        print("<", line.rstrip(b"\r").decode("utf-8", "replace"))

def send(l):
    print(">", l)
    s.sendall(l.encode() + b"\r\n")

if pw:
    send("PASS " + pw)
send("CAP LS 302")
send("NICK " + nick)
send("USER %s 0 * :%s client" % (nick, nick))
send("CAP END")
drain(3)
send("JOIN #e2e")
drain(3)
send("PRIVMSG #e2e :hello")
drain(2)
send("LISTCHANS")
drain(2)
send("PLAYBUFFER #e2e")
drain(2)
send("QUIT :bye")
