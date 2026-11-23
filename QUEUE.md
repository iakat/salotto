# QUEUE.md — feature parity work: ZNC + The Lounge

What it would take to reach feature parity with **ZNC** (bouncer) and **The
Lounge** (web client), and what we deliberately refuse from each. This is a work
queue, ordered. Every item states its evidence: files read in the upstream
source, counts from that source, and what parity actually means in behaviour.

Counted from source in `/opt/data/cache/scratch/znc-src` (ZNC, cloned from
`github.com/znc/znc`) and `/opt/data/cache/scratch/lounge-src` (The Lounge
4.6.0-pre.2, cloned from `github.com/thelounge/thelounge`) on 2026-11-22.

---

## Part 0 — Ground rules

1. **No feature ships without the behaviour it protects.** Parity is measured in
   what a user can do and what a log file says, never in "we have a module with
   that name".
2. **We do not import The Lounge's defects.** §Part 3 lists, with a citation for
   each, the thirteen complaints the ArchiveTeam wiki records about The Lounge.
   Each is either designed out or explicitly refused; none may be reintroduced by
   "parity" in a later milestone.
3. **Admin exists, but only inside our own Web UI** (katia: "we want a concept of
   Admin and Admin Panel but that should come with our built in Web UI client").
   No ZNC-style `webadmin` as a separate skin over the same data, no
   second-hand admin surface to keep in sync.
4. **`salottod` stays dumb where it can.** Bouncer state that exists only to
   make a nicer UI lives in the client. The logging tracker (§ done, `lane/zlog`)
   is the one sanctioned exception and its scope is closed.
5. **Nothing here is a date commitment.** Order is dependency order.

---

## Part 1 — Bouncer parity with ZNC

ZNC ships 59 modules (`ls modules/*.cpp | wc -l` → 59) and 32 bouncer commands
(`AddCommandHelp(...)` in `src/ClientCommand.cpp` → 32). "Full parity" means
*every* one of those either exists, or is deliberately dropped with a reason
written here. Nothing gets to be quietly missing.

### 1.1 Core bouncer capabilities (must have — these are what a bouncer *is*)

| # | capability | ZNC evidence | our state |
|---|---|---|---|
| B1 | Persistent upstream socket per network, survives client detach | `CIRCSock`, `CIRCNetwork` | **have** (upstream goroutines) |
| B2 | Replay buffer per window with per-client pointer | `CBuffer`, `CChan`, `CQuery` | **have** (one ring per window, per-client cursors, `GAP` on cursor loss) |
| B3 | Attach/detach without a visible disconnect to the network | `CClient`, `CUser::AddClient` | **have** |
| B4 | Multiple concurrent clients on one session | `ListClients` | **have** (hub, `clients_attached`) |
| B5 | Multi-network per user with independent state | `AddNetwork`/`DelNetwork`/`ListNetworks` | **yes**: `facade.AddNetwork`/`RemoveNetwork`/`Networks` with a mutex-guarded sorted list, and `cmd/salottod/netmgr.go` owning the per-network lifecycle (`Start` builds rings/writer/ident/`upstream.New` and registers with the facade; `Stop` cancels, waits for the dial goroutine, closes the writer, then unregisters). `/stats` reads the manager, so a runtime network shows up immediately. Runtime networks are in-memory only until the admin surface (§2.0.2) persists them; removal forgets every per-client cursor/attach/mute/disabled key under the network. Tests: `TestAddRemoveNetworkRoundTrip`, `TestNetworksStress` (-race), `TestStartStopLeavesNoGoroutine`, `TestStatsReflectsRuntimeNetworks`. |
| B6 | SASL PLAIN + EXTERNAL (cert) | `modules/sasl.cpp`, `modules/cert.cpp`, `modules/certauth.cpp` | **yes**: PLAIN as before, plus EXTERNAL with `sasl_external = true` and `cert_file`/`key_file` — the client certificate is presented on the TLS handshake and the SASL response is ZNC's single `"+"` (`sasl.cpp:198-213`), so no username or password is sent. `TestSASLExternalSendsEmptyResponse` drives a real mutual-TLS handshake; `TestSASLExternalRequiresCertificate` pins that a missing certificate fails closed instead of falling back to PLAIN. |
| B7 | Channel re-join on reconnect | `modules/chansaver.cpp` | **have** (config `channels`) |
| B8 | PING answered by the bouncer, not forwarded | `CIRCSock::ReadLine` | **have** |
| B9 | Reconnect with backoff, unlimited attempts | `CIRCNetwork::m_uConnectCycles` | **have** |
| B10 | Buffer playback on attach | `PlayBuffer`, `PlayBuffer` triggers | **yes**: `PlayBuffer` is window-scoped (replays from each window's own cursor, `GAP` first when the ring dropped the unread range), and attach replays every known window of every network as its own `batch` (`replayAll`/`replayNetwork`). |
| B11 | Clear buffers on demand | `ClearBuffer`, `ClearAllQueryBuffers`, `ClearAllChannelBuffers` | **yes**: `CLEARBUFFER <window>` (glob via `matchWindows`, ZNC's `MatchChans` rule) plus `CLEARALLBUFFERS`, `CLEARALLCHANNELBUFFERS`, `CLEARALLQUERYBUFFERS`. Clearing advances every client's cursor past the dropped range so a clear is not reported as a `GAP`, and sequences stay monotonic so a stale cursor is a well-formed GAP, not a rewind. |
| B12 | Per-channel enable/disable | `DisableChan`/`EnableChan` | **yes.** `CChan::Disable()` only sets `m_bDisabled`, and the single place ZNC consults it is the rejoin after a reconnect (`IRCNetwork.cpp`: `if (!pChan->IsOn() && !pChan->IsDisabled())`); `CClient::AddBuffer` never checks it, so a disabled channel keeps buffering and keeps replaying. Ours matches: the flag lives in the facade (`SetDisabled`), `BufferEmpty`/`PlayBuffer` ignore it, and `upstream.Config.JoinChannels` is consulted at every dial so the disabled channel is the only one not re-joined. A kick of our own nick disables the channel (`IRCSock.cpp:734`), as in ZNC. Tests: `TestJoinChannelsSuppressesDisabledOnReconnect` (wire, three dials), `TestNoteSelfKickDisablesChannel`, `TestJoinableChannelsSkipsDisabled`, `TestDisableChanCommand`. |
| B13 | Bind host / user bind host for outbound sockets, **plus `FREEBIND`** | `SetBindHost`/`SetUserBindHost`/`ClearBindHost`/`ClearUserBindHost`/`ShowBindHost` in `src/ClientCommand.cpp`; resolution in `CIRCNetwork::GetBindHost()` (network value, else user value); applied at dial in `CIRCNetwork::Connect` → `Connect(..., GetBindHost(), ...)` | **yes**: `internal/bindhost` holds the user default, the per-network override and the `deny_set_bind_host` policy with ZNC's resolution order (network, else user, else kernel); the five wire commands are in the facade and the dialler applies the result through `dialerWithBindHost`, which sets `IP_FREEBIND`/`IPV6_FREEBIND` before bind so an address that is not local still works. A runtime network change repoints the live connection (`SetOnChange`). |
| B14 | Trusted server fingerprints (pinning beyond CA) | `AddTrustedServerFingerprint` | **yes**: `Add`/`Del`/`ListTrustedServerFingerprint` with ZNC's responses, canonical SHA-256 colon form (`internal/certhash`, matching `Socket.cpp`'s `GetSSLPeerFingerprint`), and the dialler consulting the pins *only after* PKI verification fails — `Socket.cpp:CheckSSLCert`'s rule, so a self-signed server can be used without `insecure_skip_verify` and a wrong pin still fails closed. Tests: `TestPinnedFingerprintAcceptsSelfSigned` (real TLS handshake), `TestWrongPinFailsClosed`, `TestNoPinFailsClosed`, `TestTrustedFingerprintCommands`, `TestFingerprintNormalisation`. |
| B15 | MOTD control (block/missing) | `modules/block_motd.cpp`, `modules/missingmotd.cpp` | **yes**: `motd_blocked` suppresses 375 and 372 and answers 376 with "MOTD blocked by ZNC"; `motd_missing` answers login with 422. ZNC HALTs both 375 and 372, so neither may reach a client, which `TestMOTDBlockedNeverEmits372Alone` enforces. `TestMOTDPolicy` asserts the three policies over a real socket. |
| B16 | Runtime module load/unload | `LoadMod`/`UnloadMod`/`ListMods`/`ReloadMod` | **refuse** (see R3) |
| B17 | Config save/restart from the wire | `SaveConfig`, `Restart`, `ReloadMod` | **refuse** (R4) |

**Work estimate for 1.1**: B5 (runtime network management), B10/B11/B12,
B6-EXTERNAL, B13, B14, B15. This is the real bouncer backlog; everything below
is a feature layer on top.

### 1.1.1 B13 in full: bind host, user bind host, FREEBIND (must have)

This is not a nice-to-have: it is the difference between "the bouncer dials out
as itself" and "the bouncer dials out as the address the operator owns". ZNC has
five commands and salottod currently has none.

**ZNC semantics, read from source** (`src/ClientCommand.cpp`,
`src/IRCNetwork.cpp`, `src/User.cpp`):

| command | scope | meaning |
|---|---|---|
| `SetBindHost <host>` | network | source address for this network's outbound connection |
| `ClearBindHost` | network | forget the network value, fall back to the user value |
| `SetUserBindHost <host>` | user | default source address for every network of this user |
| `ClearUserBindHost` | user | forget the user default |
| `ShowBindHost` | both | print what would actually be used |

Resolution is one function, `CIRCNetwork::GetBindHost()`: **network value if
non-empty, else user value, else empty** (kernel picks). `SetBindHost` on the
network is a no-op-with-warning if it equals the user value (line 1546), so the
network value is only ever an *override*.

`User::IsDenySetBindHost()` exists as an operator policy: with it on,
`SetBindHost`/`SetUserBindHost` are refused for that user. Keep it — self-service
means "this user may do it", not "anyone may do anything".

The value is applied at dial time only:
`CIRCNetwork::Connect` → `Manager.Connect(..., GetBindHost(), ...)` →
`CZNC::SockClose`/`CConnectBase` bind the local socket before connecting.
Nothing else uses it; there is no per-listener bind host in ZNC's core (the
`znc.cpp:1609` comparison is ZNC's own listener config, unrelated).

**FREEBIND is the point.** Without it, `SetBindHost 198.51.100.7` fails with
`EADDRNOTAVAIL` unless the address is configured on an interface. With
`IP_FREEBIND` (Linux `setsockopt(IPPROTO_IP, IP_FREEBIND, 1)`, or the global
`net.ipv4.ip_nonlocal_bind=1`), `bind()` succeeds for any address and the dial
goes out with that source. Verified on this host, uid 10000, zero capabilities,
`ip_nonlocal_bind=0`:

```
$ cc -O1 -o fbhost fb.c && ./fbhost
BOUND 192.0.2.77:52361 (freebind ok)
```

That is a per-socket `IP_FREEBIND` doing the work, not a sysctl and not a
privilege. In Go this is `net.Dialer.Control`:

```go
d := &net.Dialer{
    LocalAddr: &net.TCPAddr{IP: bindIP},
    Control: func(network, address string, c syscall.RawConn) error {
        var serr error
        if err := c.Control(func(fd uintptr) {
            if bindIP.To4() != nil {
                serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1)
            } else {
                serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_FREEBIND, 1)
            }
        }); err != nil {
            return err
        }
        return serr
    },
}
```

Note the ordering trap: `Control` runs **after** `socket()` and **before**
`bind()`, which is exactly where the option must be set. Setting it on the
`net.TCPConn` after `Dial` is too late and silently does nothing.

**Honest limitation, and it must be in the docs:** `IP_FREEBIND` makes `bind()`
succeed; it does not make the address *reachable*. The reply packets come back to
an address the host does not own, so unless the operator has the route (a
transit/anycast setup, a tunnel, or a NAT that rewrites the source) the connection
opens and then hangs. So: FREEBIND is supported, and the failure mode when the
routing is missing is "connects, then no traffic" — say that in the error path
and in `docs/SPEC.md`, do not pretend it is a magic capability. Also note
`IP_FREEBIND` needs no privilege, but a port below 1024 in `SetBindHost` (a
`host:port` form, which ZNC does not support and we should reject) would.

**Config and wire.** Two new fields, mirroring ZNC's split:

```toml
[[networks]]
name      = "freebnc"
bind_host = "198.51.100.7"   # optional; empty = kernel default

[user]
bind_host = ""               # optional default for every network
```

Wire commands, same names as ZNC so muscle memory works: `SetBindHost`,
`ClearBindHost`, `SetUserBindHost`, `ClearUserBindHost`, `ShowBindHost`. All
require the network/user admin scope; `ShowBindHost` prints the *resolved* value
and where it came from (`network` / `user` / `kernel`), because otherwise the
operator cannot tell why the dial is using what it is using. Validation at set
time: parseable IP literal (v4 or v6), reject a port, reject a hostname (ZNC
resolves nothing here; a hostname would silently become one address at random).
`FREEBIND` is always on for a non-empty bind host — no separate flag, because a
bind host that cannot bind is not a feature.

Tests: resolution order (network > user > kernel), clear-falls-back, denied user
refused, `EADDRNOTAVAIL` gone with FREEBIND (bind to `192.0.2.77` in a test that
uses `Control`), and the negative control — without `Control`, the same test
**must** fail with `EADDRNOTAVAIL`, which is the proof the option is what makes
it work and not a coincidence of the test host.

### 1.1.2 ident (`:113`) — ZNC's `identfile`, but ours

IRC servers ask `identd` on port 113 "who owns this TCP pair", and refuse or
mark connections without an answer. ZNC answers this with `modules/identfile.cpp`,
which works by *rewriting an external ident daemon's spoof file* (`ispell`/
`oidentd`) — a hack around not owning port 113. We own the process, so we can be
the ident server.

Refuse the ZNC approach (writing another daemon's config file is a race with a
process we do not control) and do it directly:

- Listen on `tcp :113` (config `ident_listen = ":113"`, default **off**). Verified
  we can: uid 10000, `ip_unprivileged_port_start=0`, bind succeeded.
- RFC 1413 request `<server-port> , <client-port>` → reply
  `<same pair> : USERID : UNIX : <ident>`. Verified round trip:
  `6667 , 40123 : USERID : UNIX : salotto`.
- Answer **only** for pairs we actually opened: the daemon must register
  `(localAddr:port, remoteAddr:port)` at dial time and delete it on close. Any
  other query gets `ERROR : NO-USER`. This is the whole safety property — a
  blanket `USERID` reply is an information leak about every socket on the host.
- Ident string: config `ident = "salotto"`, default the configured nick of the
  network that owns the connection. Never the OS user.
- Timeout 5 s per query, max 2 concurrent queries per connection, no logging of
  the queried ports beyond a counter (it is metadata about the operator's
  sessions).
- If `:113` is taken, log once at startup and continue; ident is not fatal.

This is a bouncer capability, not a module: it belongs in the daemon because the
knowledge "which sockets did *we* open" only exists there.

### 1.2 ZNC modules worth having (the 59, triaged)

Grouped, because 59 rows is a wall, and the grouping *is* the decision.

**Keep — implement (14)**

| module | what it does | why |
|---|---|---|
| `log` | ZNC-format logs | **done** in `lane/zlog` |
| `chansaver` | persist channel list | **done** — `cmd/salottod/chansaver.go`: our own JOIN adds, our own PART removes (`chansaver.cpp:69-92`); JOIN/PART only, because ZNC has no kick handler there — kicks are B12's disable flag |
| `perform` | send commands on connect | **done**, before joins (`perform.cpp` `OnIRCConnected`) |
| `keepnick` | retry the nick you want | **done**, 30 s loop (`keepnick.cpp Start(30000)`) |
| `stickychan` | keep channels joined | subsumed by chansaver + rejoin |
| `simple_away` | auto-away on detach | **done** — `internal/facade/away.go`: away after `away_wait` with no client attached, back on attach, never fighting a user-set away (`simple_away.cpp` `m_bClientSetAway`/`m_bWeSetAway`) |
| `awaynick` | change nick when away | **refuse** — retired in ZNC itself: the module's `OnLoad` returns false with "retired module - see https://wiki.znc.in/awaynick". Implementing it would be inventing behaviour upstream deleted |
| `awaystore` | store and replay away messages | **done (the useful half)** — `internal/facade/awaystore.go`: the message *store* is redundant because per-window rings plus cursors already replay private queries; what was missing is the accounting, so private PRIVMSG/ACTION that arrive while we set the away are counted and the returning client is told "Welcome back!" / "You have N messages!" (`awaystore.cpp:388-389`). No encrypted on-disk store: that is a password-protected file whose whole purpose is to survive ZNC losing state, and rings + `savebuff` already do |
| `buffextras` | put joins/parts/modes into the replay buffer | **done** by B2's routing split; `TestBuffExtrasReachesTheReplayRing` |
| `savebuff` | persist the buffer across restarts | the "M5 mmapped rings" item, in effect |
| `clearbufferonmsg` | clear buffer when you speak | **done** (`clearbufferonmsg.cpp`) |
| `stripcontrols` | strip control codes before logging | relevant to `log` |
| `sasl` | SASL PLAIN | **have** |
| `cert` / `certauth` | SASL EXTERNAL via client cert | B6 |

**Refuse — do not implement (9, with reasons)**

| module | why not |
|---|---|
| `modperl`, `modpython`, `modtcl` | a bouncer that loads scripts is a bouncer that grows state and an attack surface. R3. |
| `webadmin` | replaced by our own Web UI admin (Part 2). R2. |
| `dcc`, `bouncedcc` | DCC passthrough is a file-transfer protocol inside a chat protocol; we are not building an FTP. If needed, a separate service. |
| `identfile` | writes an ident file for a *host* we may not own; the container case makes it meaningless. |
| `listsockets` | an `ss` invocation, not a feature. |
| `raw`, `send_raw`, `shell`, `admindebug` | arbitrary command execution from a chat client. Absolutely not. |
| `lastseen` | misnamed in this queue's earlier draft. `modules/lastseen.cpp` tracks `CUser` accounts, not IRC nicks: `OnClientLogin`/`OnClientDisconnect` call `SetTime(GetUser())` and the only command is an admin-only `Show` table of every user's last login. That is the multi-tenant admin surface we refuse (`ListUsers`, R2). It would tell you nothing about when a nick was last active. |

**Defer — decide later, not v1 (the remaining ~37)**

`alias`, `autoattach`, `autocycle`, `autoop`, `autoreply`, `autovoice`,
`blockuser`, `cert` (extras), `clientnotify`, `controlpanel`, `corecaps`,
`crypt`, `ctcpflood`, `cyrusauth`, `disconkick`, `fail2ban`, `flooddetach`,
`imapauth`, `kickrejoin`, `missingmotd`, `modules_online`, `nickserv`,
`notes`, `notify_connect`, `route_replies`, `sample`, `samplewebapi`,
`saslplainauth`, `schat`, `watch`.

Each of these is a genuine feature; none is required for a bouncer to be a
bouncer. They go into the backlog one at a time, with the behaviour that motivates
them, not as a "parity checklist". One deserves an early note:

- `fail2ban` / `ctcpflood` / `flooddetach` — these are anti-abuse. They are the
  reason a public bouncer survives; schedule them with the exposure decision, not
  with the convenience features.

### 1.3 Bouncer commands (the 32)

`AddPort AddServer Attach Broadcast ClearAllBuffers ClearAllChannelBuffers
ClearBindHost ClearMOTD ClearUserBindHost Connect DelServer Detach DisableChan
Disconnect EnableChan ListAllUserNetworks ListChans ListClients ListMods
ListNetworks ListServers ListUsers LoadMod MoveChan MoveNetwork ReloadMod Restart
SaveConfig SetBindHost SetBuffer Traffic UnloadMod`

Mapping:

- **Implemented**: `Attach`, `Detach`, `Connect`, `Disconnect`, `ListNetworks`,
  `ListServers`, `ListChans`, `ListClients`, `SetBuffer`, `Traffic`,
  `ClearAllBuffers`, `ClearAllChannelBuffers`, `ClearAllQueryBuffers`,
  `ClearBuffer`, `PlayBuffer`, `DisableChan`, `EnableChan`, and the B14/B15
  command set. The list commands answer with ZNC's `CTable` grid
  (`internal/facade/table.go`) and the exact sentences from
  `ClientCommand.cpp`; `Connect`/`Disconnect` drive a real
  `m_bIRCConnectEnabled` gate in `upstream.Conn`, and `SetBuffer` resizes the
  window's ring (`winring.Set.Resize`) rather than only answering. All eleven
  cases are settled differentially against a deployed ZNC 1.10.2
  (`test/e2e/parity.py`, `docs/E2E.md`).
- **Implemented (the last four)**: `AddServer`, `DelServer`, `MoveChan` and
  `MoveNetwork`. The first three needed a server list that did not exist, so
  `internal/upstream/servers.go` added one: an ordered list seeded from the
  config address, which the dial path walks on failure (`IRCNetwork`'s server
  rotation) and `LISTSERVERS` renders. A single-address config behaves exactly
  as before. `MoveChan` orders the saved channel list — `channelSet` now keeps
  insertion order instead of sorting, because sorting would silently undo every
  move. `MoveNetwork` is **refused**: it moves a network between ZNC *users*
  (ClientCommand.cpp:700-730, admin-only) and salotto is single-user, so the
  answer says that rather than printing a usage line for a command that could
  never work.
- **Correction**: an earlier revision of this table called the above
  "already meaningful for us". Grepping the tree showed `LISTNETWORKS`,
  `LISTSERVERS`, `LISTCLIENTS`, `TRAFFIC`, `CONNECT`, `DISCONNECT` and
  `SETBUFFER` did not exist at all. The row was wrong in the optimistic
  direction; that is worse than wrong, because it stops anyone looking.
- **Must have, and not negotiable** (katia): `SetBindHost`, `ClearBindHost`,
  `SetUserBindHost`, `ClearUserBindHost`, `ShowBindHost` — plus FREEBIND support.
  These are the outbound *source address* controls, not listener config; the
  earlier draft of this file wrongly filed them with `AddPort`. Full design and
  the FREEBIND mechanism are in §1.1.1. `AddPort` alone stays out: it configures
  a per-user IRC *listener*, which is the ZNC deployment model we are not copying
  (one process, one listener config). `ClearMOTD` is `block_motd`'s toggle, kept
  as B15.
- **Refuse**: `LoadMod`, `UnloadMod`, `ReloadMod`, `Restart`, `SaveConfig`,
  `Broadcast`, `ListMods`, `ListUsers` (cross-user; no multi-tenant admin over
  IRC — admin lives in the Web UI, per Part 0.3).

So: 25 commands implemented and verified, 1 refused by nature (`MoveNetwork`,
single-user), 1 dropped by design (`AddPort`), 6 refused (module/admin surface).

### 1.4 Bouncer gaps the `log` work exposed

These came out of actually implementing ZNC-format logging and are worth
recording because they are not obvious from a feature list:

- **Q1. Membership tracking vs. dumbness.** ZNC's log module knows channel
  membership because the bouncer knows it. We now track it in the logger. If a
  second consumer ever wants membership, it must read the same tracker rather
  than grow a second one. (Guard rail already written into `lane/zlog`.)
- **Q2. `echo-message` and double logging.** ZNC logs outbound lines from the
  user→IRC path. We do too, and must suppress the server's echo or every own
  message is logged twice. Handled in `lane/zlog`; any future outbound path must
  go through the same hook.
- **Q3. Netsplits.** The Lounge's complaint #5 (Part 3) is that netsplits destroy
  scrollback. ZNC handles them by fanning `QUIT` to every shared channel, which
  we now do. A *condensed* rendering is a client concern and must not truncate
  history to be pretty.
- **Q4. Every routed line was parsed twice — done.** Routing
  (`dispatchLine` → `logfile.WindowFor`) used to call `Format` to decide the
  window, and the writer's drain called `Format` again to build the log entry.
  Ad-hoc measurement on `PRIVMSG #c`: `Format` once = 250.4 ns/op, 112 B/op,
  9 allocs; twice = 595.9 ns/op, 240 B/op, 19 allocs, i.e. ≈ +345 ns, +128 B,
  +10 allocs per logged line. Fixed in the B2 change: routing now calls
  `logfile.Route` once and hands the `[]Entry` to the writer (`Writer.OnRouted`),
  so the drain never re-parses. Re-measured after the change on this host
  (8 vCPU, Go 1.27.4): `Format` once 300.8 ns/op / 152 B / 9 allocs,
  `Format` twice 693.1 ns/op / 304 B / 18 allocs, `Route` (parse + observe,
  what a routed line now costs) 442.8 ns/op / 184 B / 10 allocs. So a routed
  line pays one parse plus the observe the drain used to do, and the second
  parse — the ≈ +345 ns half — is gone.
- **Q5. Routing and observation must not be split across goroutines.** Moving
  the parse onto the reading goroutine (Q4) left `Tracker.Observe` in the drain,
  which is asynchronous. A `JOIN` could therefore be unobserved when the next
  `PRIVMSG` in that channel was routed (the message fell to `status`), and a
  `QUIT` observed after its own membership was erased lost its fan-out. Parsing
  and observing now happen together in `Writer.Route`, in arrival order, and the
  drain skips a line routing already observed. Any future consumer that needs
  membership must go through the same call, not a second tracker.

---

## Part 2 — Web UI parity with The Lounge

The Lounge's shape, counted from source: 23 runtime dependencies
(`package.json` → `dependencies`), **75** Vue components under `client/components/`
(36 at the top level; the rest in subdirectories), **20** message-type renderers
under `client/components/MessageTypes/`, **29** socket events
(`client/js/socket-events/*.ts`), **29** IRC event handlers on the server
(`server/plugins/irc-events/*.ts`), auth plugins `local` + `ldap`, storage plugins
`sqlite` + `text`, and an uploader plugin.

### 2.0 The UI stack: one model, three renderers (charmbracelet, no duplication)

Requirement: TUI via charmbracelet, a Web UI that is **all monospace** and reuses
the charmbracelet look as much as possible, **without duplicating code**.

The rule that makes that possible: **the UI is a `tea.Model`, and nothing in it
knows what it is drawing into.** Input arrives as `tea.Msg`, output is a string
plus ANSI. Three front ends feed the same value:

| front end | input | output | how |
|---|---|---|---|
| **SSH TUI** | a real terminal over SSH | terminal | `wish` + a `tea.Program` we own, on port 2222 |
| **Web** | browser keystrokes over WebSocket | HTML | call `Update()` ourselves, pass `View()` through ANSI→HTML |
| **headless** (tests, bots) | synthetic `tea.Msg` | string compare | call `Update()` in a loop |

Verified, not assumed — same `tea.Model`, three drivers, one file of model code:

- `tea.WithInput(io.Reader)` / `tea.WithOutput(io.Writer)` / `WithoutRenderer()`
  exist in bubbletea v1.3.10, so a `Program` can be driven with no TTY at all.
- A model driven by hand (`m, _ = m.Update(msg)` … `m.View()`) produces the same
  string the `Program` would render: `VIEW=\x1b[1;38;5;212m<me>\x1b[0m hi\n> yo`.
  Same value still satisfies `tea.Model` for a real `Program`.
- lipgloss v1.1.0 renders a titled bordered box, then
  `terminal.Render()` (buildkite/terminal-to-html **v3**, `go 1.22`) turns it into
  HTML with **zero ESC bytes left** and 13 `<span class="term-*">` elements:

```
<span class="term-bgx99"> </span><span class="term-fgx231 term-bgx99 term-fg1">salotto</span>…
<span class="term-fgx63">╭──────────────────────────────────────╮</span>
<span class="term-fgx63">│</span><span class="term-fgx240">20:03:43</span> …
```

That is the whole trick: **the browser does not get a second UI implementation,
it gets the terminal rendering.** Monospace is free (it *is* a terminal grid),
the charmbracelet look is free (it *is* lipgloss), and there is no CSS theme to
keep in sync because the only CSS is terminal-to-html's 9.8 KB
`terminal.css` (embedded, 240 `term-fgx*` classes) plus the layout for our own
chrome around the terminal pane.

**Two traps, both found by probing, both must be avoided:**

1. **24-bit colour is silently dropped.** `terminal-to-html` v3 maps SGR 8-bit
   (`38;5;n`) to `term-fgx*` classes, but its 24-bit branch is an empty `case`
   ("outside the scope"). A truecolor lipgloss style therefore renders as
   unstyled text in the browser. So the daemon sets
   `lipgloss.SetColorProfile(termenv.ANSI256)`: every style must be expressible
   in the 256-colour palette. Confirmed: with `TrueColor` the `#DDD` body text
   lost its colour; with `ANSI256` it becomes `term-fgx188` and survives.
   Pin this with a test that greps the rendered HTML for `38;2` and fails.
2. **A daemon has no TTY, so lipgloss would end up with the ASCII profile** and
   strip all colour before the HTML converter ever sees it. The explicit
   `SetColorProfile` is what makes the SSH and web outputs *identical*.

**Dependency budget**, one Go module, all `go 1.23`-or-lower so the existing
`go 1.24.0` toolchain is enough: `bubbletea v1.3.10`, `lipgloss v1.1.0`,
`bubbles v0.21.1`, `wish v1.4.7`, `charmbracelet/ssh`, `terminal-to-html/v3
v3.14.0`, plus the already-present `vellum`. That is the whole UI stack: about
seven modules, no Node, no bundler, no second language, no `npm audit`.

The browser side is then thin enough to be honest about: one WebSocket that
speaks the existing `salotto.v1` frames, one `<pre>` target, a keystroke encoder
and a resize observer. It is glue, not a client — and it is the same reason
`docs/SPEC.md` §13 already says the client is transport-agnostic.

**Status: the model, the three drivers and the wire are all real.** What was
missing for a while was the last one: the model and every renderer were complete
and tested, and both front ends faithfully drew an empty screen because nothing
connected either of them to a bouncer. `internal/ui/transport.go` is that half
(socket, per-window routing, `tea.Msg` emission) and `internal/ui/program.go` is
the bridge that drives one model from it; `cmd/salotto-ssh` and `cmd/salotto-web`
both take `-bouncer`. Proven against the deployed bouncer in
`hermes-generated-content`, both directions, by `test/e2e/probe_ui.py` and
`test/e2e/probe_ssh.py` — see `docs/UI-E2E.md`. Still open in this section: the
WebSocket (W1) and the PWA shell (§2.0.1), which change the transport and not the
model.

### 2.0.1 PWA, first class

The Web UI ships as an installable PWA from the first release, not bolted on:

- `manifest.webmanifest`: name `salotto`, `display: standalone`, monospace-only
  icon (a glyph, not a photo), theme colour = the terminal background.
- **Service worker**: cache the shell (HTML, `terminal.css`, the ~30 KB glue JS,
  the icon set) so the app opens instantly and shows the **cached scrollback**
  read-only when the network is down, with a banner saying it is stale. It must
  never pretend to be connected: sending while offline is disabled, not queued
  silently.
- **No offline mutation.** A bouncer with a queue of unsent commands that fire
  hours later is a footgun; show the failure instead.
- **Web Push** (W7) is the only reason a service worker does work in the
  background, and it stays off by default with VAPID keys generated by the
  daemon on first use.
- Install prompt handled explicitly: no interstitial, just a menu item.
- iOS quirk to document: no push without "Add to Home Screen", so the UI must
  detect standalone mode and say so instead of offering a button that cannot
  work.

### 2.0.2 Self-service

"Self-service" is a bouncer requirement, not a web-UI nicety: a user must be able
to create their own networks, channels, bind hosts and settings without the
operator editing a file, and without becoming an admin.

- **OIDC login** (`docs/SPEC.md` §8 already specifies it) → a salotto user record
  keyed by `iss` + `sub`, created on first login. No password store of ours.
- First login creates a **user** with no networks and a landing screen whose only
  affordance is "add a network".
- Per-user limits, configurable and enforced server-side: networks, channels per
  network, buffer memory, upload bytes, log retention. Limits are the operator's
  policy; hitting one is a clear message, not a silent failure.
- Everything ZNC exposes as a per-user command is available here: `SetBindHost`
  (subject to `IsDenySetBindHost`), `SetUserBindHost`, nick/altnick/ident/realname,
  SASL credentials, quit message, auto-join list, per-channel buffer on/off.
- **Secrets live server-side and are write-only over the wire**: SASL passwords,
  server passwords and ident strings are `PUT`-able and never `GET`-able, only
  shown as `set`/`unset`. (The same rule as this repo's other work: a password
  that can be read back is a password that leaks through a screenshot.)
- **Deleting a user deletes their logs and uploads**, and says exactly how many
  files that is before doing it. No "orphaned data" surprise.
- No admin escalation path: a user cannot grant themselves `bnc.admin`; that is
  an operator action in the config file or in the admin panel (§2.3).

### 2.0.3 File uploads

IRC has no file transfer that survives a bouncer well, so uploads are ours:
paste a file, get a URL, and the URL is posted to the channel as a normal
message.

- Storage: a directory per user under `upload_dir`, files named by
  content-addressed hash, never by the client-supplied name (that name is
  metadata only). One metadata row per file: size, MIME, uploader, channel,
  timestamp, expiry.
- Limits: per-file and per-user-total quotas, both configurable; quota check
  happens on the streaming write, not after, so a huge upload cannot fill the
  disk before being rejected.
- Content policy, explicit: no executable MIME types served inline, always
  `Content-Disposition: attachment` unless the type is in a small allowlist
  (image/png, image/jpeg, image/gif, image/webp, text/plain, application/pdf),
  `X-Content-Type-Options: nosniff` always, MIME sniffed from bytes and
  disagreeing with the extension → attachment + a warning to the uploader.
- Serving: opaque unguessable ID, no directory listing, `Cache-Control: private`,
  and **expiry is the default** (e.g. 30 days, configurable, `0` = never). A
  bouncer that silently becomes permanent public file hosting is a liability.
- Deleting a message does not delete the file (a bouncer must not lose data), but
  the file list is per-user and visible, so it can be deleted deliberately.
- The upload goes over the same authenticated session, not a separate token
  scheme, so revocation is one action.

### 2.1 What we build, in order

| # | item | notes |
|---|---|---|
| W1 | **Transport adapter**: same frames over a binary WebSocket (`Sec-WebSocket-Protocol: salotto.v1`) | already designed in `docs/SPEC.md` §13; the whole client is transport-agnostic by construction |
| W2 | **Client state engine**: channels, nicklists, topics, modes, unread, scrollback — built from checkpoint + replay | this is the TUI's core, reused; the UI is a view over it, not a second implementation |
| W3 | **Channel + query rendering**, message types for join/part/quit/nick/kick/topic/mode/ctcp/away/invite/error/whois | The Lounge has 20 message-type renderers; that is the honest count of shapes to draw, and each is small |
| W4 | **Search**: cross-channel, with jump-to-context | The Lounge's search is single-channel and cannot jump to context (#3 in Part 3). Ours is server-side over the log index (`/v1/log/search`, done) *plus* in-client buffer search. Two different questions, two different mechanisms. |
| W5 | **Link previews, done safely** | see 2.2 — this is where The Lounge is most wrong |
| W6 | **File uploads** | a separate concern with its own storage, quota and content-type policy; not an IRC feature |
| W7 | **Push notifications** via Web Push | optional, off by default; requires HTTPS + VAPID keys |
| W8 | **Typing indicators, mentions, mute, highlight rules** | IRCv3 `typing`, client-side mentions |
| W9 | **Themes, keybinds, per-window settings** | pure client, no daemon change |
| W10 | **Admin panel** (see 2.3) | our own, inside the Web UI, not a `webadmin` clone |

Dependency discipline: the daemon stays Go + `vellum`. The browser client is the
only place a JS toolchain appears, and it is a build artifact — no runtime JS
dependency in the served app beyond what we ship. That is the opposite of The
Lounge's 23 runtime deps in the server.

### 2.2 The three things we do differently from The Lounge, by design

1. **Link previews fetch server-side, on demand, with an explicit allowlist and a
   cache.** The Lounge's prefetching is the single worst thing on the ArchiveTeam
   list: enabled globally, no per-channel or per-pattern control, so a busy
   channel turns every user's client into a request amplifier against the linked
   host (issue #4805). Ours: **a preview is only fetched when a human asks for
   it**, never on render; the fetch is subject to an SSRF guard (no loopback, no
   RFC1918, no link-local, no `file:`), a size cap, a content-type allowlist, and
   a positive-result cache. Embedded remote media is *not* a feature; it is a
   privacy leak with a checkbox. If we embed, we proxy and we cache — the Lounge's
   own config comment admits the cache is what avoids leaking the user
   (defaults/config.js L125-143).
2. **History is never deleted automatically.** The Lounge's "low-value message"
   pruning (#4) is an attitude we refuse outright. Retention is a policy the
   operator writes down, per deployment, and the default is keep.
3. **Storage layout is inspectable.** The Lounge stores messages as JSON inside a
   `TEXT` column (#5). Ours: plain text log files in ZNC's tree, derived index
   files next to them, both readable with `grep` and `cat`. A format you cannot
   read with standard tools is a format you cannot debug at 3 a.m.

### 2.3 Admin and Admin Panel (required, our own)

Scope, deliberately small:

- **Identity**: OIDC resource server on the daemon (`docs/SPEC.md` §8 already
  specifies this — discovery, JWKS, audience check, tickets). Admin is a scope
  (`bnc.manage` + `bnc.admin`), not a separate auth path.
- **Admin panel surfaces**: networks and their connection state; sessions and
  attached clients with the ability to detach; buffer sizes and clear; per-user
  limits within the deployment ceiling; log index health (files, index age,
  rebuild); **bind-host resolution per network (and whether FREEBIND is actually
  in effect — the resolved value plus the origin, §1.1.1)**; **ident listener
  state (`:113` bound or not, query counter, last refusal reason, §1.1.2)**;
  **upload storage usage per user and expiry sweep status (§2.0.3)**; daemon
  version and uptime. **No** module loading, **no** arbitrary config editing,
  **no** shell, **no** restart button.
- **Audit**: every admin action appends a line to an admin log with actor `sub`,
  action, target, result, timestamp. Admin actions are the one thing a bouncer
  must be able to answer "who did that" about.
- **Boundary**: the admin panel is served by the same binary, on the same origin,
  behind the same OIDC session. There is no `webadmin` module, no second UI, no
  separate cookie domain. That is the whole point of Part 0.3.

---

## Part 3 — Explicitly refused, with citations

From `https://wiki.archiveteam.org/index.php/User:JustAnotherArchivist/The_Lounge`
(read 2026-11-22), the thirteen recorded defects. Each is a **design
constraint on us**, not a bug report we inherit:

| # | The Lounge defect | our constraint |
|---|---|---|
| 1 | Link prefetch global-only, no per-channel/pattern control; request amplification (#4805) | preview only on explicit human action; allowlist; cache; §2.2.1 |
| 2 | Remote media embedded directly unless server-side caching is enabled; leaks user details (config L125-143) | never embed remote media inline; proxy+cache or nothing |
| 3 | Search single-channel, cannot jump to result context, slow for heavy users | server-side cross-window search with offsets + context jump (§2.1 W4) |
| 4 | History deletion of "low-value messages" aimed at "data hoarders" (#4799) | no automatic deletion, ever; retention is an explicit operator policy |
| 5 | SQLite schema stores messages as JSON in a TEXT field (#407 in `messageStorage/sqlite.ts`) | plain files + derived index; `grep` must work |
| 6 | Cannot rotate through multiple servers of one network; breaks with round-robin DNS (#966) | `AddServer`/`DelServer`/`MoveNetwork` in the backlog (B-side §1.3); DNS round-robin must work by default |
| 7 | Out of disk space → reconnection loop (#4571) | a disk-full log write is a counted failure, not a disconnect; the socket never dies because a file cannot be written |
| 8 | `/quit` deletes the network from config, undocumented (#4194) | `/quit` quits; deleting a network is an explicit, separate, confirmed action |
| 9 | Netsplits destroy scrollback; condensing is purely cosmetic while the scrollback limit still applies (#8 in the wiki list) | quit fan-out to every shared channel (done in `lane/zlog`); condensation must never truncate history |
| 10 | `/msg` to an unopened window is not logged without `echo-message` (#590, #4538) | outbound logging goes through a dedicated hook (`OnSend`), so it does not depend on the server echoing |
| 11 | Long messages split inefficiently, UI does not indicate the split | split at a stated width, and mark continuation lines in the UI |
| 12 | Certain connect failures (refused, timeout, reset, ircd connection limits) produce no reconnect (#4842, #4889, #4103, #3419, #3244) | every failure mode reconnects with backoff; the only stop is an explicit user action |
| 13 | Public mode: refresh after a disconnect lands on the default channel, not the one you were in (#4949) | the client persists its active window per session and restores it; the URL is the source of truth |

Two more principles, beyond the list:

- **Public/read-only mode is a mode, not a deployment.** Any view the UI can
  show, it must be able to show read-only, with the same code path (§2.1 W1's
  frame carries the capability, the server enforces it).
- **No silent data loss of any kind.** Every one of the thirteen items above is,
  at bottom, a case of the software discarding something the user expected to
  keep: history, a message, a channel, a connection. That is the invariant.

---

## Part 4 — Order of work

Dependency order, not priority-by-excitement. Each line is roughly one milestone.

1. **Finish `lane/zlog`** — ZNC-format logs, index, search endpoint. *(in flight)*
2. **B13 + FREEBIND + ident** (§1.1.1, §1.1.2). Small, self-contained, and
   nothing else depends on it — but it changes how connections are made, so it
   must land before anything else touches the dial path.
3. **B2**: per-window buffers, per-client cursors, `GAP` on cursor loss
   (`docs/SPEC.md` §6 already specifies it). Everything else in the client needs
   this.
4. **B5 + B10–B12**: runtime networks, buffer playback per window, clear,
   enable/disable per channel.
5. **UI stack skeleton** (§2.0): **done** — `internal/ui` holds one `tea.Model`
   with three drivers (`cmd/salotto-ssh`, `cmd/salotto-web`, `ui.Headless`), and the
   ANSI→HTML path is proven over real HTTP. See `docs/UI.md`.
   the ANSI→HTML path proven end to end on one screen. Do this *before* OIDC, so
   the model's shape is set by a rendering that already works in both places.
6. **OIDC + tickets** (`docs/SPEC.md` §8) and **self-service** (§2.0.2) — the gate
   for any Web UI.
7. **W1–W3**: WebSocket transport, client state engine, rendering. First usable
   web UI, and the PWA shell (§2.0.1) with it.
8. **W10 + §2.3**: admin panel and audit log.
9. **B6-EXTERNAL, B14, B15** — **done**. SASL EXTERNAL with a client
   certificate, trusted server fingerprints, MOTD block/missing. The
   `Keep — implement` module set below is the last piece of this line.
10. **W4–W9**: search UI, previews (safely), **uploads (§2.0.3)**, push,
    typing/mentions, themes.
11. **Anti-abuse** (`fail2ban`, `ctcpflood`, `flooddetach`) if the deployment is
    ever exposed beyond its owner.
12. **Deferred backlog** (§1.2) one item at a time, each with its motivating
    behaviour.

### What "done" means for parity

Not "all 59 modules". It means: a person who uses ZNC today can move to
`salottod` without losing a capability they actually use, and a person who uses
The Lounge can move without losing a capability *and* without inheriting any of
the thirteen defects. Anything beyond that is a feature request, and it comes
with an argument.
