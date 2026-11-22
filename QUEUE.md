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
| B2 | Replay buffer per window with per-client pointer | `CBuffer`, `CChan`, `CQuery` | **partial**: one ring per network, no per-window split, no per-client cursor in the POC |
| B3 | Attach/detach without a visible disconnect to the network | `CClient`, `CUser::AddClient` | **have** |
| B4 | Multiple concurrent clients on one session | `ListClients` | **have** (hub, `clients_attached`) |
| B5 | Multi-network per user with independent state | `AddNetwork`/`DelNetwork`/`ListNetworks` | **partial**: config-file networks only, no runtime add/remove |
| B6 | SASL PLAIN + EXTERNAL (cert) | `modules/sasl.cpp`, `modules/cert.cpp`, `modules/certauth.cpp` | **partial**: SASL PLAIN only |
| B7 | Channel re-join on reconnect | `modules/chansaver.cpp` | **have** (config `channels`) |
| B8 | PING answered by the bouncer, not forwarded | `CIRCSock::ReadLine` | **have** |
| B9 | Reconnect with backoff, unlimited attempts | `CIRCNetwork::m_uConnectCycles` | **have** |
| B10 | Buffer playback on attach | `PlayBuffer`, `PlayBuffer` triggers | **partial**: replays whole ring, no window selection |
| B11 | Clear buffers on demand | `ClearBuffer`, `ClearAllQueryBuffers`, `ClearAllChannelBuffers` | **no** |
| B12 | Per-channel enable/disable logging-to-buffer | `DisableChan`/`EnableChan` | **no** |
| B13 | Bind host / user bind host for outbound sockets | `SetBindHost`, `SetUserBindHost` | **no** |
| B14 | Trusted server fingerprints (pinning beyond CA) | `AddTrustedServerFingerprint` | **no** |
| B15 | MOTD control (block/missing) | `modules/block_motd.cpp`, `modules/missingmotd.cpp` | **no** |
| B16 | Runtime module load/unload | `LoadMod`/`UnloadMod`/`ListMods`/`ReloadMod` | **refuse** (see R3) |
| B17 | Config save/restart from the wire | `SaveConfig`, `Restart`, `ReloadMod` | **refuse** (R4) |

**Work estimate for 1.1**: B2 (per-window buffers + per-client cursor), B5
(runtime network management), B10/B11/B12, B6-EXTERNAL, B13, B14, B15. This is
the real bouncer backlog; everything below is a feature layer on top.

### 1.2 ZNC modules worth having (the 59, triaged)

Grouped, because 59 rows is a wall, and the grouping *is* the decision.

**Keep — implement (14)**

| module | what it does | why |
|---|---|---|
| `log` | ZNC-format logs | **done** in `lane/zlog` |
| `chansaver` | persist channel list | currently config-only; needs runtime channels (B5) |
| `perform` | send commands on connect | one small parser, high value |
| `keepnick` | retry the nick you want | two words of state, avoids identity drift |
| `stickychan` | keep channels joined | subsumed by chansaver + rejoin |
| `awaynick` | change nick when away | needs an away concept (B18) |
| `awaystore` | store and replay away messages | pairs with B18 |
| `simple_away` | auto-away on detach | pairs with B18 |
| `buffextras` | put joins/parts/modes into the replay buffer | matters once B2 splits windows |
| `savebuff` | persist the buffer across restarts | the "M5 mmapped rings" item, in effect |
| `clearbufferonmsg` | clear buffer when you speak | a client preference, cheap |
| `stripcontrols` | strip control codes before logging | relevant to `log` |
| `sasl` | SASL PLAIN | **have** |
| `cert` / `certauth` | SASL EXTERNAL via client cert | B6 |

**Refuse — do not implement (8, with reasons)**

| module | why not |
|---|---|
| `modperl`, `modpython`, `modtcl` | a bouncer that loads scripts is a bouncer that grows state and an attack surface. R3. |
| `webadmin` | replaced by our own Web UI admin (Part 2). R2. |
| `dcc`, `bouncedcc` | DCC passthrough is a file-transfer protocol inside a chat protocol; we are not building an FTP. If needed, a separate service. |
| `identfile` | writes an ident file for a *host* we may not own; the container case makes it meaningless. |
| `listsockets` | an `ss` invocation, not a feature. |
| `raw`, `send_raw`, `shell`, `admindebug` | arbitrary command execution from a chat client. Absolutely not. |

**Defer — decide later, not v1 (the remaining ~37)**

`alias`, `autoattach`, `autocycle`, `autoop`, `autoreply`, `autovoice`,
`blockuser`, `cert` (extras), `clientnotify`, `controlpanel`, `corecaps`,
`crypt`, `ctcpflood`, `cyrusauth`, `disconkick`, `fail2ban`, `flooddetach`,
`imapauth`, `kickrejoin`, `lastseen`, `missingmotd`, `modules_online`, `nickserv`,
`notes`, `notify_connect`, `route_replies`, `sample`, `samplewebapi`,
`saslplainauth`, `schat`, `watch`.

Each of these is a genuine feature; none is required for a bouncer to be a
bouncer. They go into the backlog one at a time, with the behaviour that motivates
them, not as a "parity checklist". Two deserve an early note:

- `lastseen` — trivial to do well and genuinely useful, but it is *state about
  people*, so it needs a retention rule before code.
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

- **Already meaningful for us**: `Attach`, `Detach`, `Connect`, `Disconnect`,
  `ListNetworks`, `ListServers`, `ListChans`, `ListClients`, `SetBuffer`,
  `Traffic`, `ClearAllBuffers`, `ClearAllChannelBuffers`, `DisableChan`,
  `EnableChan`, `AddServer`, `DelServer`, `MoveChan`, `MoveNetwork`.
- **Meaningless for us, drop**: `AddPort`/`ClearMOTD`/`ClearUserBindHost`/
  `SetBindHost`/`SetUserBindHost` — these configure a *shared* IRC listener per
  user, which is the ZNC deployment model we are not copying; our listener is one
  process config. `ClearMOTD` is `block_motd`'s toggle.
- **Refuse**: `LoadMod`, `UnloadMod`, `ReloadMod`, `Restart`, `SaveConfig`,
  `Broadcast`, `ListMods`, `ListUsers` (cross-user; no multi-tenant admin over
  IRC — admin lives in the Web UI, per Part 0.3).

So: ~18 commands to implement, 8 dropped by design, 6 refused.

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

---

## Part 2 — Web UI parity with The Lounge

The Lounge's shape, counted from source: 23 runtime dependencies
(`package.json` → `dependencies`), **75** Vue components under `client/components/`
(36 at the top level; the rest in subdirectories), **20** message-type renderers
under `client/components/MessageTypes/`, **29** socket events
(`client/js/socket-events/*.ts`), **29** IRC event handlers on the server
(`server/plugins/irc-events/*.ts`), auth plugins `local` + `ldap`, storage plugins
`sqlite` + `text`, and an uploader plugin.

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
  rebuild); daemon version and uptime. **No** module loading, **no** arbitrary
  config editing, **no** shell, **no** restart button.
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
2. **B2**: per-window buffers, per-client cursors, `GAP` on cursor loss
   (`docs/SPEC.md` §6 already specifies it). Everything else in the client needs
   this.
3. **B5 + B10–B12**: runtime networks, buffer playback per window, clear,
   enable/disable per channel.
4. **OIDC + tickets** (`docs/SPEC.md` §8) — the gate for any Web UI.
5. **W1–W3**: WebSocket transport, client state engine, rendering. First usable
   web UI.
6. **W10 + §2.3**: admin panel and audit log.
7. **B6-EXTERNAL, B13, B14, B15** and the `Keep — implement` module set.
8. **W4–W9**: search UI, previews (safely), uploads, push, typing/mentions,
   themes.
9. **Anti-abuse** (`fail2ban`, `ctcpflood`, `flooddetach`) if the deployment is
   ever exposed beyond its owner.
10. **Deferred backlog** (§1.2) one item at a time, each with its motivating
    behaviour.

### What "done" means for parity

Not "all 59 modules". It means: a person who uses ZNC today can move to
`salottod` without losing a capability they actually use, and a person who uses
The Lounge can move without losing a capability *and* without inheriting any of
the thirteen defects. Anything beyond that is a feature request, and it comes
with an argument.
