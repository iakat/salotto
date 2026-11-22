# salotto proof of concept

This is a runnable, deliberately small version of salotto. One `salottod`
process holds an IRC connection to each configured network, writes every line
it receives into a fixed-size ring, and gives a plain RFC1459 IRC client both
the historical ring contents and the live stream when that client connects. It
does not try to be a full bouncer yet: no channels, nicks, topics or modes are
tracked on the server, and there is no authentication in front of the
client-facing port.

```
your IRC client ──IRC──▶ salottod ──IRC──▶ real network
```

## Build and run

```sh
cp config.example.toml salotto.toml
$EDITOR salotto.toml       # set a unique nick, your channels, optional SASL
make build
./bin/salottod -config salotto.toml
```

The example listens on `127.0.0.1:6667`, serves stats on
`127.0.0.1:6668`, and connects to Libera and FreeBNC over TLS. Replace the
example nick before running against a real network. Use `-check` to validate a
configuration without starting anything:

```sh
./bin/salottod -check -config salotto.toml
```

Then point your usual client at `127.0.0.1:6667`. For example:

```sh
weechat -r '/server add salotto 127.0.0.1/6667 -notls;/connect salotto'
irssi -c 127.0.0.1 -p 6667 -n mynick
```

Halloy: add a server whose address is `127.0.0.1` and port is `6667`, with TLS
off. `sic` and any other client that speaks basic RFC1459 registration also
works.

## What actually works

These are the things that were exercised while building this POC:

- `go build ./...`, `go test ./... -race -count=1`, `make lint` and
  `make alloc-gate` all pass.
- `salottod` connected to `irc.libera.chat:6697` over TLS, registered, and
  joined the configured channel. `/stats` showed `state=registered`,
  `attempts=1`, and live `lines_in`/`lines_out`/ring counters.
- A plain TCP client sent `CAP LS 302`, `CAP END`, `NICK` and `USER`, received
  the 001/002/003/004/005 registration numerics, and then received the whole
  ring as a `BATCH +replay ...`/`BATCH -replay ...` replay with `time=` tags.
- The replay and the live stream are rewritten per attached client. Numerics
  are addressed to that client's own nick, a line from the upstream identity
  carries the client's nick in its prefix, the `353` names list contains the
  client's nick, and an extended-join `JOIN <chan> <account> :<realname>` is
  stripped to `JOIN <chan>` for clients that did not negotiate
  `extended-join`. A stock client therefore rebuilds `#hermes` with itself in
  the nick list instead of discarding the replayed state.
- `GET /stats` returned the documented `key=value` text over plain HTTP.
- SIGTERM cancelled the upstream context, closed the listeners, waited for the
  goroutines, logged each phase, and exited cleanly.
- The ring benchmarks report zero bytes and zero allocations per operation.

## Known limitations

- The bouncer does not track channels. A channel `JOIN`ed at runtime (not in
  the config) is lost when the upstream reconnects; only configured channels
  are joined again.
- The facade has no authentication at all. Bind it to loopback or put it
  behind an SSH tunnel. Anyone who can reach the port gets the whole session.
- The ring is lossy: it has a fixed size and overwrites the oldest records. A
  client that attaches after a gap cannot be told exactly what it missed,
  except by the jump in the replayed `time=` stamps.
- A slow client is dropped rather than buffered indefinitely.
- There is one upstream connection per network, shared by all attached
  clients. Every attached client shares that same upstream identity, so two
  clients using different nicks both see themselves as the same upstream user
  after rewriting. A message sent by one client appears to the other as if it
  came from that shared identity (with the other client's own nick), not from
  the first client's nick.
- An upstream `NICK` change is dropped rather than propagated. The facade
  advances its cached upstream nick so later lines still rewrite correctly,
  but clients only see their own nick.
- There is no nicklist, topic or mode state on the server. Clients rebuild
  those from the replayed 353/332 numerics and the live stream, so a client
  that attaches late only knows about named users whose NAMES reply is still
  inside the ring.
- TLS to the upstream verifies certificates unless
  `insecure_skip_verify = true`. There is no certificate pinning.
- The only special-cased client commands are the ones in
  `internal/facade/server.go` (`CAP`, `NICK`, `USER`, `PING`, `QUIT`, `WEBIRC`).
  Everything else is forwarded verbatim, so a command the bouncer does not
  know is the network's problem, not a bug.

## Verify it yourself

With `salottod` running and `stats_listen = "127.0.0.1:6668"`:

```sh
curl -s http://127.0.0.1:6668/stats
```

Example from a live run against Libera:

```
uptime_seconds=12
net.libera.state=registered
net.libera.attempts=1
net.libera.lines_in=70
net.libera.lines_out=6
net.libera.ring_oldest=1
net.libera.ring_newest=70
net.libera.ring_bytes=21331
clients_attached=0
clients_dropped_slow=0
goroutines=9
```

To watch the registration and ring replay without an IRC client (`nc` is not
installed on the build host, so this uses a Python socket):

```sh
python3 - <<'PY'
import socket, time
s = socket.create_connection(("127.0.0.1", 6667))
s.sendall(b"CAP LS 302\r\nCAP END\r\nNICK x\r\nUSER x 0 * :x\r\n")
s.settimeout(0.5)
end = time.time() + 3
while time.time() < end:
    try:
        data = s.recv(65535)
    except socket.timeout:
        continue
    if not data:
        break
    print(data.decode(errors="replace"), end="")
PY
```

The beginning of the output from that command in the live run:

```
:salotto CAP * LS :server-time message-tags echo-message away-notify account-notify extended-join cap-notify batch chathistory labeled-response
:salotto 001 x :Welcome to libera x!x@salotto
:salotto 002 x :Your host is salotto, running salotto-0.1
:salotto 003 x :This server was created Sun Nov 22 2026 15:18:55 UTC
:salotto 004 x salotto salotto-0.1 i s
:salotto 005 x CHANTYPES=# PREFIX=(ov)@+ CASEMAPPING=rfc1459 NETWORK=libera :are supported by this server
:salotto 375 x :- salotto Message of the day -
:salotto 372 x :- salotto proof of concept: replayed history follows.
:salotto 376 x :End of /MOTD command.
:salotto BATCH +replay libera-replay
@batch=libera-replay;time=2026-11-22T15:18:55.719Z :tantalum.libera.chat 002 salottopoc7q2 :Your host is tantalum.libera.chat[93.158.237.2/6697], running version solanum-1.0-dev
@batch=libera-replay;time=2026-11-22T15:18:55.719Z :tantalum.libera.chat 003 salottopoc7q2 :This server was created Fri Nov 13 2026 at 07:28:59 UTC
...
:salotto BATCH -replay libera-replay
```

## Benchmarks on this machine

Command:

```sh
make bench
```

Output (`AMD EPYC 9645`, go1.24.10):

```
goos: linux
goarch: amd64
pkg: github.com/iakat/salotto/internal/ring
cpu: AMD EPYC 9645 96-Core Processor
BenchmarkAppend-8       31506687        36.76 ns/op       0 B/op       0 allocs/op
BenchmarkReplay1MiB-8      27553        45341 ns/op       0 B/op       0 allocs/op
PASS
ok      github.com/iakat/salotto/internal/ring  2.915s
no facade benchmarks
```

## What the POC deliberately does not do

This is not the design in [`docs/SPEC.md`](SPEC.md), it is a probe of the core
idea. The POC has no OIDC or tickets, no binary client protocol or management
frames, no client-written checkpoints, no per-user limits or ceilings, no
persisted state across restarts, and no cursor per client. The spec describes
the system those features grow into: framing, `GAP` and `ATTACH` cursors,
checkpoint blobs, OIDC JWKS validation, per-user limits and a real TUI client.
