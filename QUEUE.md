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

**Audit, last run against the ZNC clone in `cache/scratch/znc-src`:** every one of
the 59 module names and all 32 command names appears in this file, each with a
status. Recheck with:

```sh
# every module name must appear somewhere in this file (must print nothing)
comm -23 <(ls modules/*.cpp | sed 's|.*/||; s/.cpp//' | sort) \
         <(grep -o '[a-z_0-9]\+' QUEUE.md | sort -u)

# 32 bouncer commands, counted the only way that gives 32: a bare
# `grep -c AddCommandHelp` returns 61 because the file also contains a lambda
# named AddCommandHelp and 27 plain calls, so count distinct help entries.
grep -o 'AddCommandHelp("[A-Za-z]*"' src/ClientCommand.cpp \
  | sed 's/AddCommandHelp("//; s/"//' | sort -u | wc -l        # 32
```

The bouncer side (Part 1) is closed: 59/59 modules settled, 32/32 commands
settled, and 15 differential parity cases run through a deployed ZNC 1.10.2
(`test/e2e/parity.py`, 14 identical + 1 declared deviation). What is still open
in this file is the **client** side (Part 2: W1 WebSocket, the PWA shell, search
UI, uploads, admin panel) and the one module row blocked behind it
(`adminlog`, which needs an admin surface to log). Those are not parity gaps —
they are the next milestone, and §2.0 now says which half of it is real.

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
| `chansaver` | persist channel list | **done** — `cmd/salottod/chansaver.go`: our own JOIN adds, our own PART removes (`chansaver.cpp:69-92`); JOIN/PART only, because ZNC has no kick handler there — kicks are B12's disable flag. It did not work for a long time and the e2e case said it did: the case only asserted that a parted channel stays gone, which is what a dead chansaver produces too. With a positive control (join a throwaway channel in-session, require it back after a reconnect) it failed immediately — `networkAdapter` did not implement `Nick()`, so the facade could not tell which lines were ours and the set stayed empty. Fixed in `be43d69`; ZNC and salotto now agree on both halves (kept: yes, parted came back: no). The verdict is measured at the IRCd by an observer that joins the throwaway channels and reads `NAMES`, because neither bouncer-side reading can answer it: the client stream only sees a window it is attached to, and ZNC's `ListChans` and salotto's list known different things. |
| `perform` | send commands on connect | **done**, before joins (`perform.cpp` `OnIRCConnected`) |
| `keepnick` | retry the nick you want | **done**, 30 s loop (`keepnick.cpp Start(30000)`) |
| `stickychan` | keep channels joined | **covered, not ported.** The module is a persistent `MCString` of extra channels plus a timer that re-`JOIN`s any not-on channel (`stickychan.cpp:105-124`); salotto's per-network `JoinChannels` does the same job off the channel list at every dial, and chansaver keeps that list honest. Porting the module would be a second channel list that disagrees with the first. |
| `simple_away` | auto-away on detach | **done** — `internal/facade/away.go`: away after `away_wait` with no client attached, back on attach, never fighting a user-set away (`simple_away.cpp` `m_bClientSetAway`/`m_bWeSetAway`) |
| `awaynick` | change nick when away | **refuse** — retired in ZNC itself: the module's `OnLoad` returns false with "retired module - see https://wiki.znc.in/awaynick". Implementing it would be inventing behaviour upstream deleted |
| `awaystore` | store and replay away messages | **done (the useful half)** — `internal/facade/awaystore.go`: the message *store* is redundant because per-window rings plus cursors already replay private queries; what was missing is the accounting, so private PRIVMSG/ACTION that arrive while we set the away are counted and the returning client is told "Welcome back!" / "You have N messages!" (`awaystore.cpp:388-389`). No encrypted on-disk store: that is a password-protected file whose whole purpose is to survive ZNC losing state, and rings + `savebuff` already do |
| `buffextras` | put joins/parts/modes into the replay buffer | **done** by B2's routing split; `TestBuffExtrasReachesTheReplayRing` |
| `savebuff` | persist the buffer across restarts | **refuse, matching ZNC.** It is not built in upstream either: it is a loadable module that serialises each window's buffer to disk on shutdown (`savebuff.cpp:199-217`) and replays it on boot (`:99-138`). salotto's rings are in memory by design and its history is the log files, which are already on disk and already searchable — a third copy would be a cache pretending to be history. |
| `clearbufferonmsg` | clear buffer when you speak | **done** (`clearbufferonmsg.cpp`) |
| `adminlog` | log admin actions to a file or syslog | **done.** It was recorded here as "blocked on the admin surface" and that stopped being true when `*controlplane` landed: that *is* the surface, and it has mutating commands (`TRAFFICCTL`, `SCRIPTRELOAD`). Every command is audited from the dispatch wrapper, once, so a handler cannot forget - and the one thing that made this more than bookkeeping is that a controlplane handler's return value means "the reply was sent", not "the command worked". A trail built on it would record `SHUTDOWN` as *accepted*, because a refusal is also a NOTICE that gets written. Each refusing arm therefore states its reason and the audit records that, so refusals appear as refusals with a reason (`SHUTDOWN refused: no admin authentication is configured` is the line an operator wants after an incident). Unknown verbs are recorded too - an attempt is an attempt. Two sinks that fail independently: a bounded ring so `ADMINLOG` works with no configuration, and an append-only file from `admin_log` for the half that outlives the process. An unopenable file is reported, never fatal: an audit log that can take the service down is an availability risk, not a control. Lines are flattened so a result containing a newline cannot become a second entry. Seven mutations bite, including "record every command as accepted", "derive the outcome from the reply", and "only audit successes". The syslog half is not done; `admin_log` is a file path. Original note: **open — blocked on the admin surface, not on this row.** ZNC writes user/network add/remove/edit events to `GetSavePath()+"/znc.log"` or syslog (`adminlog.cpp:99-115`). Part 2 §8 requires the same audit trail from us ("every admin action appends a line with actor, action, target, result, timestamp"), and it should be built in rather than a module you can unload. But there is no admin surface yet to log: `grep -r 'admin' internal/ cmd/` finds nothing, because the single-tenant OIDC admin panel is §2.0.2/§2.3 and not started. Writing an audit log for actions that do not exist would be a log of nothing. |
| `stripcontrols` | strip control codes before logging | **done**, opt-in. `internal/logfile/stripcontrols.go` reproduces `CString::StripControls_n` (`ZNCString.cpp:1452-1498`) including the two cases a regex gets wrong (a comma not followed by digits is text; a trailing code can still emit a comma). Applied to log bodies only when `strip_controls = true`, because in ZNC the module is opt-in and `log` writes what was said — `sanitizeBody` separately and always removes CR/LF/C0 for log integrity, which is a different job. Tests: `TestStripControlsMatchesZNC`, `TestStripControlsModuleIsOptIn`, `TestStripControlsKeepsTheReplayRingIntact`. Live proof, both halves: with `strip_controls = false` a `\x0304strip<ts> code\x03` line reaches the log as `04strip<ts> code` (C0 gone, digits kept); with `true` it reaches the log as `strip<ts> code` (whole code gone). Read off PVC `salotto-e2e-data` in `hermes-generated-content`. |
| `sasl` | SASL PLAIN | **have** |
| `cert` / `certauth` | SASL EXTERNAL via client cert | **done** as B6: `sasl_external` + `cert_file`/`key_file`, EXTERNAL response is ZNC's single `"+"` (`sasl.cpp:198-213`), no PLAIN fallback when the certificate is missing |

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
| W1 | **Transport adapter**: the browser is a terminal, not an imitation of one | **done, and redesigned.** The JSON frame protocol (`state`/`results` carrying `render` plus parallel `windows`/`nicks`/`unread`/`topic`/`previews` fields) worked and was still wrong: every consumer had to reassemble a UI from those fields, and that is where the doubled nicklist, the twice-printed input line and the box-inside-a-box came from. It was deleted. The front end is now a PTY bridge in the shape ttyd and GoTTY use — one `salotto-tui` child per websocket on its own pseudo-terminal (`internal/ui/terminal.go`), xterm.js in the browser (`assets/xterm/`, vendored, MIT). One renderer: the model's `View()`, over the wire as bytes. Window size is TIOCSWINSZ → `WindowSizeMsg`; mouse, keys, paste and colour are the terminal's own. `test/e2e/probe_public_ws.py` still proves alice's browser and bob's separate bouncer see each other through the public edge. |
| W2 | **Client state engine**: channels, nicklists, topics, modes, unread, scrollback — built from checkpoint + replay | **nicklists, topics, unread and scrollback done.** Membership from 353/366, kept current from JOIN/PART/QUIT/KICK/NICK; the frame carries the active window's list plus a per-channel count. Topics from 332/331 and live TOPIC changes, shown in the TUI title row and a browser header, following the window. The TUI draws the member list as a column inside its border, so the three front ends show the same thing rather than the browser having a sidebar the terminal lacks. Both verified through the public URLs (`probe_public_nicks.py`, `probe_public_topic.py`). Modes and member prefixes from 324 and MODE, folded to a sorted set, shown beside the topic. W2 is complete. |
| W3 | **Channel + query rendering**, message types for join/part/quit/nick/kick/topic/mode/ctcp/away/invite/error/whois | **query rendering done, and it was broken in two places at once.** Driving the real binary (not a test) found that our own PM to `alice` — relayed back by the server, as every server does — was filed under the **sender**, so it landed in a window named after ourselves and `Own` was false, drawing our own words as if alice had said them; the transport now files a private message under the other party (`if t.isSelf(from) { return target }`). The second half: `/msg alice …` drew **nothing at all** — no window, no line — because a command was exempted from the echo wholesale instead of by kind; `/msg` and `/query` now open the address's window, switch to it, and draw the body there, and `/msg #chan hi` draws `hi` in the channel (`retargetWindow`, `carriesBody`). **Also done: whois rendering.** `/whois` used to dump 311/319/312/330/317/318 raw into `status`; the family is now collected and drawn as one block, in words, in the window the question was asked from (`internal/ui/whois.go`, `noteWhois` at send time, `318` closes it). Unknown numerics still fall through raw rather than being swallowed; an unasked reply goes to `status`; two interleaved replies stay separate; idle time is `2h1m`, not `7300`. Eight mutations red. Verified live on a pty against a real `salottod`: the block reads `alice / ~a@host.example (Alice Smith) / on #salotto #dev / on irc.libera.chat (Libera Chat) / logged in as alicesmith / idle 2h1m, signed on ...`. **Still open in W3**: away/invite/kick have no dedicated rendering, and `whois` has no window of its own (it answers where it was asked, which is the more useful rule). |
| W4 | **Search**: cross-channel, with jump-to-context | **done, verified live** (`test/e2e/probe_search.py`, six checks, stable across runs). The earlier note here was wrong.** `Model.Search` had existed with no caller - no command reached it and the browser never asked - so "the in-client half is done" described a method rather than a feature. A dead API is not a feature. `/search <text>` now drives it from the terminal: hits from every window, each naming its window so `/go` reaches it, a count first ("did I spell it right"), the query echoed when there are no matches, a usage message for an empty `/search` rather than a search for the empty string, and a cap so a one-word query cannot push the conversation out of the window. Six mutations bite, including searching only the active window and dropping the window name from each hit. History: `Model.Search` crosses every window and each hit carries its window, which the browser uses to jump. Linear scan on purpose — in-memory scrollback is bounded by the window caps, so an index has nothing to buy and can disagree with the lines it indexes. The server half (`/v1/log/search`) exists for "find this from last month". Verified live by `probe_public_features.py`. **The other half is now wired, so `/search` is no longer memory-only.** The facade answers a new `SEARCH` verb from `logfile.Query` — one `NOTICE ... :SEARCH <date> <time> <network> <window> <body>` per hit, `SEARCH-END <count>`, or `SEARCH-ERR <reason>` — and the client sends it asynchronously, draws a `searching…` row, and files the rows into the same walkable picker (`TestSearchReachesTheIndexNotJustTheClient`, `TestSearchEndCarriesTheCount`, `TestSearchBodySurvivesSpacesAndColons`, `TestSearchWithLoggingDisabledRefuses`, `TestSearchRefusesAPathInWindow`, `TestTheClientFilesSearchRepliesIntoSrows`, `TestALateSearchReplyIsDropped`, `TestSearchDoesNotBlockTheRenderLoop`, `TestSearchWithoutABouncerStillScansMemory`). Each request carries an `id:` echoed as the `salotto.search` tag, so a late answer to a superseded query is dropped instead of opening a picker that looks current. **Fixed**: search reaches the bouncer's disk across a restart, not just the client's ring. **Made visible, not fixed**: the default range is the bouncer's seven days and Query's 200-row cap, and a hit that exists only on disk opens the right window but lands at its bottom because the line is not in the client's ring; the row carries its date and time instead. The local in-memory scan remains the fallback when there is no bouncer, chosen by one predicate (`searchRemote`), not a second drifting path. |
| W5 | **Link previews, done safely** | **done, and now a line rather than a card.** `internal/ui/preview.go` + `preview_ip.go`. Server-side, once per link, only for recent lines in the active window. Refuses private/loopback/link-local/reserved addresses including 169.254.169.254, re-vets every redirect hop, http(s) only, 4 s timeout, 96 KiB cap. A fetched preview arrives as `PreviewReady` and is appended to the window as a `LinePreview`: a terminal has no panels, and putting the card in the scrollback is what gives the SSH and terminal front ends previews too. It never counts as unread and never switches the active window. Mutation-tested: forcing `publicIP` to true fails eight addresses; dropping the fetch fails the delivery test. |
| W6 | **File uploads** | **done and verified live over the public host** (`test/e2e/probe_upload.py`, 18 assertions). A file is streamed in and served back from `internal/upload`, with `internal/ui/upload_http.go` as the HTTP mapping. Four things are load-bearing. The quota is enforced **during the write**: checked afterwards, the bytes are already on disk and the limit stops nothing. An oversized body is refused on its declared length **before** it is read, because cutting the stream off mid-body protects the disk but shows the user a connection reset instead of "file too large" - the live probe found that, the tests did not. The served type is what the bytes are, never the client's claim, and a claim that contradicts the bytes forces a download even when the bytes are harmless. Every upload is its own file: content addressing (the first version) made two uploads of the same screenshot one file, so the second name replaced the first and deleting either link broke the other - also found by the probe. Requiring separate uploads deleted the dedup accounting entirely. Expiry is 30 days by default and the sweep is real: the first sweep called Delete, which goes through Get, which refuses expired files, so it found nothing and reported success forever while the disk filled. **The browser half is proven the same way**: a real `File` dispatched as a `drop` on the terminal, and the URL it produced read off the input row of a live emulator - not a passing POST, because a handler could upload and then drop the answer. The URL is inserted into the input line, not sent: sending it would be a message the user did not write. |
| W7 | **Push notifications** via Web Push | **done, and verified end to end on the deployment.** PWA manifest + service worker (`internal/ui/assets/`), VAPID keys generated on first boot, subscription store on the front end's own volume, `aes128gcm` encryption written in-repo and pinned to RFC 8291's published test vector (a self-consistent scheme can be wrong in a way that answers 201 and shows nothing), ES256 JWT with `aud` = the push service origin. `/push/notify` requires a bearer token and a missing token is a refusal. The trigger lives in `dispatchRouted`: a mention while **nobody is attached** - a mention with a client watching is a line on screen. Verified live: bob mentioned alice with no client attached, the bouncer logged `push: notification sent`, the front end accepted it. Two bugs found only by running it: `parseMention` insisted on a leading `:` so the notifier was blind on every network that sends message-tags, and the notifier dropped every error silently so a 403 looked like a quiet phone. |
| W8 | **Typing indicators, mentions, mute, highlight rules** | **mentions done**: word-boundary match on our own nicks, in the transport because it is the only place that knows them. Unread and mention are separate signals (a mention wins in the sidebar; a count would compete with it for the same glance). Verified live. **mute and highlight rules done and verified live**: mute suppresses the signal not the text ("why did nobody answer" is the worse failure); a highlight word is a mention, matched whole-word by the same `wordIn`, and decided in the model rather than the transport, because a highlight set in a browser must not change what a phone finds interesting. Both were proven in the browser by reading cell colours, which is the only way - see 2.1.0. Typing indicators open: they need an upstream `+typing` capability and this bouncer's upstreams do not negotiate it, so there is nothing honest to display yet. |
| W9 | **Themes, keybinds, per-window settings** | **keybinds done; themes are now the terminal's.** The old browser theme toggle set CSS variables on a page that drew its own panels; with the page reduced to a terminal there are no panels to theme, and colour belongs to the program (lipgloss, 8-bit SGR) and the emulator's palette. Alt-T/J/I/K/C binds still work because bubbletea handles them. **settings are now held by the bouncer, as one opaque blob** (`PREF GET`/`PREF SET`: base64 settings JSON, persisted to `<state_dir>/prefs.json` when configured, fanned out to every attached client and never parsed). **Fixed**: a `/fset` change in the TUI reaches a browser tab, and the value survives moving machines because the bouncer keeps it; the old per-process file was the architecture that made those impossible. **Made visible, not fixed**: each client still writes its own file first as the offline copy for a plain IRCd or a local-only run, so a client that has not reconnected can hold a stale copy until its next `PREF GET`; and the daemon cannot validate, migrate or explain a key it does not parse, so a malformed blob is the client's problem. **A cap the reader could not reach was a data-loss bug, now fixed** (and its sibling, below): the blob cap was 64 KiB while the reader cut every client line at `max_line_len` (8192), and base64 cut on a four-character boundary still decodes - so at some byte alignments the store kept a fragment of the settings, every client failed to parse it, and every client silently reverted to the defaults, while at others the user got a `NOTICE` blaming the encoding. The reader now assembles a line up to the cap (`(MaxPrefsBytes+2)/3*4 + 1024`, shared constant, not a second knob), dispatch applies `max_line_len` to every verb except `PREF`, a cut `PREF SET` is refused rather than decoded, and the client refuses locally what the bouncer would reject so the file keeps the change and the status line says "saved here, not shared". Tests: `TestALargeLegalBlobIsAcceptedWhole`, `TestATruncatedPrefsLineIsRefusedNotStored` (both alignments), `TestPrefsCapIsReachableThroughTheReader`, `TestAnOversizedBlobIsRefusedLocally`; seven mutations red, including the restored original bug and a refusal that still stores the fragment. **A dropped connection is now said out loud**: nothing told the model when the stream ended - the driver's pump stopped and the last frame stayed on screen still reading "connected", so a user typed into a dead link and the line sat there looking sent. The transport now sends one `Disconnected{Reason}` before closing (no `net.ErrClosed` case: `Close` shuts `done` first, so `push` already drops it, and one guard that also stops a panic beats two), the model marks itself offline, writes the reason into the status window as an error and says "offline" in the bar, and a submit while disconnected is refused in the window the user typed in rather than echoed. `Send` reported every unqueued line as "connection is not keeping up", a congestion message that is a lie on a closed socket, and worse, `push` sent on the closed channel - the first keystroke after a bouncer restart panicked the client. Tests: `TestADroppedConnectionSendsDisconnected`, `TestAReasonIsNotInventedWhenWeClosedIt`, `TestTheModelAdmitsItIsDisconnected`, `TestTheModelRefusesToSendWhenDisconnected`, `TestSendingOnADeadConnectionDoesNotPanic`; four mutations red, verified against a real `salottod` killed with SIGKILL. **The client now reconnects, and a replay no longer doubles the scrollback**: item 12's rule is that every failure reconnects with backoff and only an explicit user action stops it, so the retry loop lives in the transport (`run`, `readOnce`) instead of in each of the three drivers, and `Msgs()` now means "this client is finished", not "this attempt failed" - a closed channel is what the pump and the drivers already understood. The reconnect repeats the handshake from credentials kept at the first `Register`, announces a drop and a return, and reports a refused socket (the one message that suggests the bouncer is not running) once a minute rather than once a second.

Reconnecting without deduplication is worse than not reconnecting: a bouncer replays its whole ring on registration, so the client re-received everything it already held and the scrollback grew by the size of the ring on every reconnect - measured at 3 lines in, 3 replayed, 6 on screen. The marker is the bouncer's own tag: a replayed line arrives inside `BATCH +/-replay` and carries `batch=<id>`, and no live line does. Two earlier versions were wrong and both are worth recording, because each looked fine in unit tests. **Ordering is not the marker**: the first version opened suppression at the reconnect and closed it at 376, assuming the burst ends after the replay, and a probe against a built daemon read `376` at line 8 with the first `BATCH +replay` at line 9 - the window was already shut when every replayed line arrived, so the feature was dead code in production. **The timestamp is not the marker**: a live line's `time` is stamped at fan-out, a replayed line's out of the ring, measured 17.242 against 17.241 for the *same message*, so a time comparison fails for every line. Identity within a batch is nick, verb and text in one window, and it is a multiset: the first `baseline` copies a batch delivers are the ones already on screen and the rest is news the client missed. The baseline is fixed at batch start and not recomputed per line - recomputing counts the batch's own copies, which measured as "a batch of three over a window holding one left two" - and the counter is per batch, so a second replay reconciles from scratch. A live line is never compared, because the bouncer never tags one: an untagged repeat is a server saying the same thing twice.

Tests: `TestTransportReconnectsInsteadOfDying` (the assertion it replaced asserted the opposite, and that behaviour was the bug), `TestClosingTheClientStopsTheRetrying`, `TestTheHandshakeIsRepeatedOnTheNewConnection`, `TestAnAttemptThatCannotOpenASocketIsAnnounced`, `TestAReplayedLineIsNotDrawnTwice` (the measured 3-to-6 case), `TestATimestampIsNotPartOfALineIdentity` (the live-probe bug, as a regression), `TestALiveLineIsNeverDeduplicated`, `TestAReplayNeverEatsARealRepeat`, `TestAReplayOfADifferentLineIsKept`, `TestABatchReconcilesTheWindowToWhatTheBouncerHolds`, `TestTheReplayIsRecognisedEvenThoughTheBouncerEndsTheMotdFirst`, `TestOnlyABatchedLineIsMarkedAsAReplay`, `TestABatchIdReachesTheModel`, `TestAPlainServerSReplayIsStillLiveTraffic`, `TestReconnectStateIsVisibleButNotNoisy`, `TestAFailedAttemptIsReportedOncePerMinute`; nine mutations red; verified against a real `salottod` twice - once with the client connection dropped while the daemon lived, and once by capturing the daemon's actual registration burst and feeding those bytes back through the transport's parser and the model, which is the check that found the ordering bug. **The hidden limit is gone**: the duplicate check used to search only the last 512 lines of a window, which made a long reconnect quadratic and wrong - the daemon replays a window's whole ring, so the first lines of a long replay matched nothing inside the cap and were drawn again (measured: a 2000-line window came back as 4000). The baseline is now a tally of the window taken once per batch, so each replayed line is a map lookup and there is no cap. A capped scan also *lost* lines: a batch of 700 identical lines over a window holding none delivered 699. No `salotto.seq` tag is needed for this, which retires the shared-line objection that killed two earlier designs - a line shared by two windows still has one tag, and the tally is per window anyway. **Still local**: rendering and layout - the blob is applied by the client that draws it - and there is no per-network setting; one blob is the whole namespace. **The settings screen no longer wipes a rule list**: Enter/Space called `set(key, "")`, which toggles a boolean and *clears* a text row, so pressing space on highlights or mutes deleted every entry - locally, and (once the blob was shared) on every other client too. Text rows now say how to edit themselves in the screen's hint row instead of destroying the list, the hint is cleared on any cursor move so it cannot describe the wrong row, and the footer says "held by the bouncer, drawn here" because "client, not bouncer" stopped being true when the bouncer started holding the blob. Tests: `TestSpaceOnATextRowDoesNotWipeIt` (both lists), `TestABooleanRowStillToggles` as the negative control, `TestTheHintDoesNotOutliveTheRow`, `TestTheSettingsFooterDoesNotClaimTheStateIsLocal`; five mutations red. **The screen can now edit a rule list where it shows it**: every text row used to answer "edit with /fset", which is honest and teaches a user the screen is a read-only summary of something set elsewhere - and the screen is the discoverable half of the settings, the command being the scriptable half. A text row opens an editor in place (enter commits, escape abandons, the caret sits where the value lives), and while it is open a space is a space in the value, which is what makes the old destructive keystroke unrepresentable rather than merely unlikely; arrows do nothing rather than jump rows, because a jump would redirect the rest of the typing into a different setting. A list could also only grow: the one removal was `none`, the whole list, so a value with a leading `-` now removes a single word and says so when the word was not there. Both paths go through `Settings.set`, so the screen and `/fset` cannot drift into two spellings of the same setting. Tests: `TestATextRowOpensAnEditorInsteadOfSayingGoElsewhere`, `TestEscapeAbandonsAnEdit`, `TestSpaceInTheEditorDoesNotClearTheList`, `TestABooleanStillTogglesOnSpace`, `TestAWordCanBeRemovedWithoutClearingTheList`, `TestRemovingAWordThatIsNotThereSaysSo`, `TestNoneStillClearsEverything`, `TestTheCaretIsDrawnWhereTheValueIs`, `TestMovingWhileEditingDoesNotLeaveTheField`; ten mutations red. **per-window settings open, and deliberately so**: `/fset` is the client's settings and they are global to that client, because the thing weechat makes per-window is mostly layout and a terminal has one layout. What a window really owns here - mute and highlight - is a rule list keyed by nick or word, which is a different axis. Revisit if a user asks for a per-channel trigger. **The window list itself is now appearance, and it is `/fset`-able**: `group_by_server` draws the list under a heading per server with the windows sorted inside each group, and `buffer_numbers` prefixes every window with a stable weechat-style number, both defaulting to on because they were asked for ("ordered the channel list and be under server name", "each buffer should have a number like weechat"). The flat arrival-order list of before is what `group_by_server=off` returns, drawn by the same renderer. `/b <n>` and `/buffer <n>` switch by number from `modelCommands`, so `/help` and Tab derive them rather than a hand-edited string; a bare `/b` lists the numbers, because the sidebar is a setting and is not drawn at all in a narrow terminal. **A client that dropped keys it did not recognise was deleting a newer client's settings**: the blob is shared across builds - a laptop a release behind, a tab on trunk - and `decodeSettings` only knew the fields this build has, so an older client read a newer blob, changed one boolean and pushed back a document with every unfamiliar key gone, and the next fan-out carried the loss to everyone. The older client looked innocent, because it had never seen the key. Keys with no field now live in `Settings.unknown` as raw JSON (so a value of any shape round-trips byte for byte) and are spliced back on save, sorted so two clients with the same settings write the same bytes; the known-key set is derived from the struct tags by reflection rather than hand-kept, because a second list to update is a list that goes stale silently. The same preservation applies to the file, which is the offline copy and the thing a user upgrades with. Tests: `TestAKeyThisBuildDoesNotKnowSurvivesASave`, `TestAForeignKeyOfAnyShapeRoundTripsByteForByte`, `TestAnUnknownKeySurvivesAFileRoundTripToo`, `TestTwoClientsWithTheSameSettingsWriteTheSameBytes`, `TestAnUnexportedFieldIsNotOnTheWire`; five mutations red, and the end-to-end case was reproduced against the real bouncer (an unpatched older client destroys the newer key; a patched one keeps it). |
| W10 | **Admin panel** (see 2.3) | **the command surface is done; the panel is not.** `*controlplane` is a second namespace alongside `*status` - client-scoped vs operator-scoped, so the access check that does not exist yet has exactly one place to go: VERSION, RUNTIME, CLIENTS, NETWORKS, USERS, CONFIG, TRAFFICCTL, SCRIPTS, SCRIPTRELOAD, SCRIPTLOG, ADMINHELP, plus deliberate refusals (`SHUTDOWN`: a remote off switch with a friendly name). The OIDC session and the HTML panel remain, blocked on §2.0.2. |

Dependency discipline: the daemon stays Go + `vellum`. The browser client is the
only place a JS toolchain appears, and it is a build artifact — no runtime JS
dependency in the served app beyond what we ship. That is the opposite of The
Lounge's 23 runtime deps in the server.

### 2.1.0 Mute and highlight: what the ANSI reset costs

W8's mute and highlight rules are in and verified, but the live proof took three attempts for a
reason worth keeping: a lipgloss render ends with an ANSI reset, so colouring the nick by
splicing the rendered string into an already-styled line ends that line's style at the nick.
Every message body was drawn in the terminal default, and a mention highlight set on the line was
wiped by the nick that precedes the text. The model was right in every unit test; the frame
"contained escape codes" in every assertion; only reading cell colours out of a real terminal
showed it.

Rule for this codebase: **never splice a rendered fragment into a rendered string.** Render the
pieces separately and let each re-establish the enclosing style (see `nickStyled`). A test for a
colour has to assert the colour is still in effect *after* the last reset, not that a sequence
appears somewhere in the line.

### 2.1.1 Two networks, one channel name: the window namespace — **fixed**

**Every network has a window called `status`.** The bouncer keys windows by `(network, name)`
and sends each network's registration replay in its own batch (`ch-test-status-replay`,
`freebnc-status-replay`). The client keyed windows by name alone, so both replays landed in one
window and whichever arrived last won.

Measured on the live two-network deployment before the fix:

- Alice's status window showed **freebnc's** 001-005 replay; `ch-test` appeared in it nowhere.
- A `401 No such nick` from ch-test **was** in the bouncer's `ch-test-status-replay` ring and
  **was** delivered live to a raw client. It was not on the TUI's screen.

That was a data-visibility bug, not a display preference: one network's status window was
unreachable, so errors, MOTDs and bouncer replies arriving there were invisible.

**The fix is a window key that carries the network.** `windowKey(network, name)` returns
`"network/name"` when the network is known and the bare name when it is not, so a client of a
plain IRCd keeps exactly the naming it always had. `keyWindow` splits it back and `winName`
gives the display name. Every map the model keeps about a window - `windows`, `order`,
`unread`, `mention`, `topics`, `modes`, `nicks`, `bufferNo`, `netOf` - is keyed by
that, so two networks' `status` are two windows with two buffer numbers and two scrollbacks.

Three details are load-bearing:

- **Migration, not assumption.** A window often exists before its server is named: the client
  attaches, a bare `status` window takes an untagged line, and only then does a tagged line
  reveal the network. `noteNetwork` rekeys the window through `migrateKey`, which moves every
  map at once, because a half-moved window is a conversation with the right lines and the
  wrong nicklist. The buffer number moves with it - the user has already learnt it.
- **The key never reaches the user or the wire.** The title row, the sidebar rows, the buffer
  list and search hits show `winName`. `CHATHISTORY` gets the bare name, because the bouncer
  resolves the network itself and a request for `libera/status` would return an empty batch,
  which reads as "no history" rather than as a bug. The same goes for the transport's
  `SetActive`.
- **Ambiguity is refused, not guessed.** When two networks hold `status`, a bare `status` no
  longer names one window, and `netOfName` returns "" rather than picking one. The buffer list
  and search hits print `status (libera)` only when a name is duplicated, and that printed form
  is a valid `/go` target so the list never names a window nobody can type back in.

The tests are on the lines, not the sidebar: a sidebar that draws two rows over merged
scrollback is a display fix on a data-loss bug, which is the mistake an earlier revision of
this entry made. `TestTwoNetworksKeepSeparateWindowsOfTheSameName` asserts each window holds its
own line and not the other's, mid-migration identity keeps its number and scrollback, the
printed `name (network)` form round-trips through `/go`, and a single-network client grows no
parentheses. Twelve mutations were confirmed red, including the four that survived a first
pass: ambiguous-name guessing, the title leaking the key, the sidebar row leaking the key, and
`CHATHISTORY` carrying the key.

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

## Part 2.4 — Scripting (not in the original queue; asked for and built)

A sandboxed scripting layer, hot-loaded, no restart.

**Starlark, not Python-in-Go.** A Python-in-Go worth the name would have `import os` working
unless someone remembered to break it, and "someone remembered" is not a sandbox. Starlark has
no imports, no filesystem, no network, no clock, no `os`, no goroutines - the reachable world is
exactly what the daemon predeclares, which is `state` and `log`. That list is enumerable in one
file, and the file is `internal/script/engine.go`.

**Three guarantees, in order of how bad it is to lose them**, each proved by mutating the guard:

1. **No crash.** Every entry into script code is recovered, including a Go-level panic from a
   buggy builtin - a different failure from a Starlark error, and the one the recover above the
   call exists for. Removing the inner recover takes the test binary down.
2. **No hang.** A step budget checked inside Starlark's own interpreter loop stops `while True:
   pass` in milliseconds, plus a wall-clock deadline that cancels a hook which never reaches a
   step check. The cost of a wedged hook is one abandoned goroutine, not a wedged daemon.
3. **No silent failure.** Load errors stay in the list with their message; hook errors go into
   the same ring `log()` writes to. This one was broken in production and fixed: the error
   counter was incremented and no command printed it, so `SCRIPTLOG` said "no script output yet"
   while a hook failed on every seventh line.

**Hot reload is a content hash, polled every two seconds.** Three detectors were wrong before it
worked, all in the same deployment: the mtime of the *loaded* scripts (can only change after a
reload), `DirEntry.Info` on a ConfigMap volume (an lstat - it reports the symlink, which never
changes, while the file behind it is replaced), and mtime+size after `os.Stat` (a swap can land
in the same clock tick). The bytes are the only thing that means "the script changed", and these
files are a few hundred bytes. Polling rather than fsnotify for the same ConfigMap reason: an
inotify watch can end up on a dead inode and then look exactly like "hot reload does not work".

**Verified live**, not just in tests: the demo script was edited in the Flux repo, the ConfigMap
was swapped by the kubelet, the running daemon reloaded at 03:30:41 with **zero restarts** and
no dropped upstream, and the new log line appeared. A deliberate `fail()` and then a `while
True: pass` were both injected, both reported, and the daemon served lines throughout.

Hooks cannot change traffic yet: they are given values and their return value is discarded. A
first version that can rewrite traffic is a first version whose first bug rewrites traffic.

## Part 3 — Explicitly refused, with citations

From `https://wiki.archiveteam.org/index.php/User:JustAnotherArchivist/The_Lounge`
(read 2026-11-22), the thirteen recorded defects. Each is a **design
constraint on us**, not a bug report we inherit:

| # | The Lounge defect | our constraint |
|---|---|---|
| 1 | Link prefetch global-only, no per-channel/pattern control; request amplification (#4805) | preview only on explicit human action; allowlist; cache; §2.2.1 |
| 2 | Remote media embedded directly unless server-side caching is enabled; leaks user details (config L125-143) | never embed remote media inline; proxy+cache or nothing |
| 3 | Search single-channel, cannot jump to result context, slow for heavy users | **fixed**: `/search` opens a walkable result list - arrows move, enter jumps to the hit with context around it, escape leaves without undoing the jump, and any other keystroke closes the list and goes to the input - and the list now comes from the bouncer's log index (`SEARCH` + `logfile.Query`) instead of only the client's ring, so it crosses a restart and every window the bouncer has logged. **Made visible, not fixed**: the default range is the bouncer's seven days with Query's 200-row cap, and a disk-only hit opens the right window but lands at its bottom, with the date and time shown in the row; a hit still needs `enter` to leave the picker (there is no browser-side result view). |
| 4 | History deletion of "low-value messages" aimed at "data hoarders" (#4799) | no automatic deletion, ever; retention is an explicit operator policy |
| 5 | SQLite schema stores messages as JSON in a TEXT field (#407 in `messageStorage/sqlite.ts`) | plain files + derived index; `grep` must work |
| 6 | Cannot rotate through multiple servers of one network; breaks with round-robin DNS (#966) | `AddServer`/`DelServer`/`MoveNetwork` in the backlog (B-side §1.3); DNS round-robin must work by default |
| 7 | Out of disk space → reconnection loop (#4571) | a disk-full log write is a counted failure, not a disconnect; the socket never dies because a file cannot be written |
| 8 | `/quit` deletes the network from config, undocumented (#4194) | `/quit` quits; deleting a network is an explicit, separate, confirmed action |
| 9 | Netsplits destroy scrollback; condensing is purely cosmetic while the scrollback limit still applies (#8 in the wiki list) | quit fan-out to every shared channel (done in `lane/zlog`); condensation must never truncate history |
| 10 | `/msg` to an unopened window is not logged without `echo-message` (#590, #4538) | outbound logging goes through a dedicated hook (`OnSend`), so it does not depend on the server echoing |
| 11 | Long messages split inefficiently, UI does not indicate the split | split at a stated width, and mark continuation lines in the UI — **done** in `internal/ui/split.go`: `MaxSendBytes` = the bouncer's 8192-byte line limit minus room for `PRIVMSG <target> :` and tags, split at the last word boundary in the window (hard split when there is none), never inside a rune, each part marked `(i/n) ` *inside the message body* because a PRIVMSG has no out-of-band slot the other end renders. Only PRIVMSG/ACTION is split: a MODE or KICK reason cut in two is a second command the server rejects, which is worse than the limit. The model echoes the same parts the wire will carry (one `messageParts` call, not a mirror) so the local echo confirms per part instead of leaving ghosts. Escape hatch: a `PRIVMSG` this client sends is at most 7717 bytes, so it clears the daemon's 8192 reader with room for tags |
| 12 | Certain connect failures (refused, timeout, reset, ircd connection limits) produce no reconnect (#4842, #4889, #4103, #3419, #3244) | every failure mode reconnects with backoff; the only stop is an explicit user action | **done** - upstream side in `internal/upstream/conn.go`, client side in `internal/ui` (`Transport.run`: 1s doubling to 30s, stopped only by `Close`), with replay reconciliation so a reconnect does not double the scrollback. See W9. |
| 13 | Public mode: refresh after a disconnect lands on the default channel, not the one you were in (#4949) | **fixed**: the active window travels in the settings blob as `last_window` (a window key), so the bouncer carries it like any other setting and a client adopts it on `PREF`. Restored only by a client that has not chosen a window yet, so two attached clients never drag each other; an empty value leaves the client where it is |

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
