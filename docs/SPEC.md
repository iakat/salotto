# salotto — spec v0.1

A replacement for The Lounge: one Go bouncer that holds the IRC connections,
one client that runs anywhere. The bouncer is deliberately dumb — it keeps
sockets, a replay buffer and a pointer per client, and nothing else. Every bit
of IRC understanding lives in the client.

Status: draft for review. Nothing implemented.

Naming is a proposal: `salotto` (the parlour next to `stanze`). Binaries
`salottod` and `salotto`.

---

## 1. Goals

- **Persistent IRC presence.** Sockets live in the bouncer. Clients come and go;
  the network never sees a disconnect, no matter how many clients attach or how
  often.
- **The bouncer does not understand IRC.** It does not know what a channel, a
  nick, a topic or a mode is. It frames raw lines, numbers them, keeps a
  bounded ring of them, and replays from a cursor. Anything that needs meaning
  happens in the client.
- **Zero allocations in the steady-state hot path** in the bouncer (§3).
- **First-class OIDC.** The bouncer is an OAuth2 resource server. No passwords,
  no local user database, no session cookies of its own invention.
- **The client can manage the bouncer within server-enforced ceilings** (§7):
  add and remove networks, set per-network options, attach and detach. It can
  lower limits, never raise them past the ceiling.
- **One binary each, no cgo,** stdlib plus `golang.org/x/oauth2` (client only) and `github.com/blevesearch/vellum` (plus three transitive deps: mmap-go, bitset, golang.org/x/sys) for the log index in salottod.

## 2. Non-goals (v1)

- No client-side message search beyond the daily log index (§2.1); no push notifications.
- No web UI. The client is a TUI first; a browser client is a later transport
  adapter (same frames over a binary WebSocket, §13).
- No shared channel access between users. One user, many sessions, many
  clients. Multi-tenant means many users, each in their own box.
- No bouncer-side IRC state in the *facade*: the facade keeps no IRC state. A read-only tracker inside the logging subsystem (`internal/logfile`) keeps membership and topic solely to reproduce ZNC's fan-out; it is never exposed to clients, runs on the log drain goroutine, and is cleared on disconnect. This is a narrowing of the rule, not its removal.
- No history older than the ring. Backfill beyond the ring is the network's
  job via `chathistory` (§11), from the client.

## 2.1 Logging and search (salottod)

salottod keeps ZNC `log` module parity for log files only. Log path template
(network module):

```
<log_dir>/$NETWORK/$WINDOW/%Y-%m-%d.log
```

`$NETWORK` is the configured network name verbatim; `$WINDOW` is lowercased
with `/` and `\` replaced by `-`. One line per event, `[%H:%M:%S] ` + body,
appended and `\n`-terminated. Bodies and fan-out match ZNC exactly, with one
documented deviation: a QUIT or NICK change for a nick sharing no channel is
written to the `<network>/status/` window instead of being dropped. Bodies are
sanitized: `\r`, `\n` and C0 control bytes (except `\t`) are stripped before
writing, always (ZNC makes this optional via `-sanitize`; salotto does it by
default because a log line with an embedded CR is a corrupt log).

A per-network bounded queue feeds one drain goroutine; `OnLine` performs a
single non-blocking send and never blocks the socket. Daily indexes are built
with `github.com/blevesearch/vellum`, one `.idx` file per log file, next to it.

Index lifecycle: at writer start (and again on `SetNick`, where the network
becomes usable) one background pass indexes every log file that already exists
for every day *before* the current one, skipping any `.idx` that is newer than
its `.log`. It logs one `Info` line per window with the day and source size. The
current day is never indexed eagerly: it is incomplete by definition and would
be rebuilt on every write, so it is indexed lazily on query. The query path
builds a missing `.idx` (and rebuilds a stale one, using the same `ModTime`
rule) instead of silently falling back to a full scan; a missing index is a
cache miss to fill once, not an error. The startup pass and lazy queries may
build the same index concurrently; the writer writes a temporary file and
renames it so a reader never sees a partial index.

Search endpoint:
`GET /v1/log/search?network=&window=&q=&since=&until=&limit=&order=` on its
own listener `log_listen`. `GET /v1/log/windows?network=` lists windows.

Auth: `log_token` set requires `Authorization: Bearer <token>` compared with
`crypto/subtle`; non-loopback `log_listen` with empty token refuses to start;
loopback without token needs no auth.

Config keys: `log_dir` (empty disables logging), `log_queue` (default 4096),
`log_listen` (empty disables search), `log_token`.

## 3. What "zero allocations" means here

"0 allocs" is a property of the steady state, not of the process. Stated
precisely, and gated in CI:

| path | budget |
|---|---|
| network line → ring → N attached clients | 0 allocs, 0 B/op |
| client SEND frame → network write | 0 allocs |
| replay from ring to one client (any length) | 0 allocs |
| client frame parse (HELLO/AUTH excluded) | 0 allocs |
| attach/resume handshake | ≤ 3 allocs |
| session open, connect, TLS handshake | unbounded, cold path |
| OIDC token validation, management frames | unbounded, cold path |

Excluded from the claim, honestly: allocations inside `crypto/tls` record
processing, kernel copies, and the `runtime` itself. The claim is about user
code, and it is measured with `testing.AllocsPerRun` and
`-benchmem`, not asserted in prose.

How the budget is kept (the rules that make it possible, all of them load
bearing):

- Preallocated arenas, not slices that grow. Every buffer is sized at session
  or connection open and reused for the life of the connection.
- No `fmt`, no `strconv` on the hot path. Integer formatting goes through a
  fixed 20-byte scratch buffer.
- No `[]byte`→`string` conversions. Routing tables are keyed by `uint32` ids
  allocated at connect time; names are compared with `bytes.Equal`.
- No `map` lookups on a hot path. A slab plus intrusive lists; the four small
  maps in the process (session by id, session by name) are touched only on
  attach and on management frames.
- No channel per line. Wakeups use one buffered `chan struct{}` of size 1 per
  attachment, sent to non-blocking; a blocking channel send allocates a sudog,
  a non-blocking send into a non-full buffer does not.
- No `defer` inside the read/write loops, no closures built per iteration.
- Logging on the hot path writes into a fixed buffer with a prebuilt
  `slog.Logger` whose attrs are created at open. No `slog.String` per line.
- Errors are sentinel values compared by identity, or small `uint16` codes in
  the frame header. No `fmt.Errorf` on the hot path.

## 4. Processes, trust boundaries

```
      OIDC issuer ──JWKS──┐
                          │
  client A ─TLS──┐        │
  client B ─TLS──┼──▶ salottod ──TLS──▶ irc.libera.chat
  client C ─TLS──┘        │        ──TLS──▶ irc.ircnet.org
                          │
                    ring per session,
                    cursor per client
```

- **salottod** holds the network sockets, the rings, the cursors, the limits,
  the JWKS cache. It is the only component that ever sees an IRC credential.
- **salotto** (client) holds all IRC state: channels, nicklists, topics, modes,
  unread marks, rendered text. It can be killed at any moment with no loss
  except its own view; it resumes from its cursor.
- The bouncer never trusts a client for anything but its own session. Every
  frame carries the session id it refers to and is checked against the user
  bound to the connection.

## 5. Wire framing

16-byte header, little endian, then payload:

```
 0        4        5        6        8                16
 +--------+--------+--------+--------+----------------+---------+
 | len u32| type u8| flags u8| code u16|    seq u64     | payload |
 +--------+--------+--------+--------+----------------+---------+
```

- `len` = payload length only. Max 64 KiB client→bouncer, 8 KiB bouncer→client.
- `seq` = the ring sequence this frame refers to: set on `LINE`, `GAP`; echoed
  on `ACK`; 0 elsewhere.
- `code` = refusal/notice code (§7.3), 0 when unused.
- `flags` = bit 0 `MORE` (replay continues), bit 1 `LOCAL_ECHO` (this line is
  the client's own, no `echo-message` from the server).

Frame types:

| type | name | direction | payload |
|---|---|---|---|
| 0x01 | `HELLO` | C→S | `u16 ver`, `u16 flags`, `u8 issuer_len`, issuer bytes |
| 0x02 | `AUTH` | C→S | bearer token bytes |
| 0x03 | `AUTH_OK` | S→C | `u16 ticket_len`, ticket, `u64 expiry_unix` |
| 0x04 | `ATTACH` | C→S | `u32 session_id`, `u64 cursor` (0 = from oldest) |
| 0x05 | `ATTACHED` | S→C | `u32 session_id`, `u64 oldest_seq`, `u64 newest_seq`, `u64 cursor` |
| 0x06 | `LINE` | S→C | `u32 origin`, raw line without CRLF |
| 0x07 | `GAP` | S→C | `u64 from_seq`, `u64 to_seq` (lines lost) |
| 0x08 | `SEND` | C→S | `u32 session_id`, `u32 client_line_id`, raw line without CRLF |
| 0x09 | `SEND_OK` | S→C | `u32 session_id`, `u32 client_line_id`, `u16 refused_code` |
| 0x0A | `ACK` | C→S | `u32 session_id`, `u64 cursor` (advance my pointer) |
| 0x0B | `MANAGE` | C→S | `u8 op`, `u32 session_id`, TLVs |
| 0x0C | `MANAGE_OK` | S→C | `u8 op`, `u16 code`, TLVs |
| 0x0D | `CHECKPOINT` | C→S | `u32 session_id`, `u64 covers_seq`, opaque blob |
| 0x0E | `CHECKPOINT_GET` | C→S | `u32 session_id` |
| 0x0F | `STATUS` | S→C | `u32 session_id`, fixed struct: state, lag_seq, attach count, bytes used |
| 0x10 | `PING` / 0x11 `PONG` | both | `u64 echo` |
| 0x12 | `ERROR` | S→C | `u16 code`, UTF-8 text (cold path, may allocate) |
| 0x13 | `BYE` | both | `u16 code` |

TLV = `u8 tag`, `u16 len`, `bytes`.

A raw line is exactly what the server sent, minus CRLF, one frame per line —
including `PING`, `NOTICE`, `ERROR`, numerics, everything. `SEND` gets CRLF
appended by the bouncer and written to the socket as-is.

## 6. Replay: ring, sequence, cursor, checkpoint

### 6.1 Ring

One ring per session, allocated at session open:

- Segments: `nSeg` fixed-size byte arenas, default 8 × 256 KiB = 2 MiB.
- A segment is a packed sequence of records `[u16 len][bytes]`, walked by
  offset. No per-line index array: the segment's first sequence number is
  enough to find a cursor's segment, then the walk is linear and cache-friendly.
- Writer appends to the current segment; when full it moves to the next,
  overwriting the oldest. **No memmove on wraparound** — that is the point of
  segments.
- Lines longer than the segment are truncated to the segment size and marked in
  the header flags.

`seq` is monotonic per session, starting at 1. `oldest_seq` and `newest_seq` are
published in `ATTACHED` and `STATUS`.

### 6.2 Cursor

A cursor is just a `u64`. The bouncer keeps one per attachment (the "pointer"
the brief asks for) and, optionally, one persisted "last known good" per user
so a fresh client on a new device resumes where the old one stopped.

On `ATTACH`:

- cursor within `[oldest_seq, newest_seq]` → replay from that seq.
- cursor older than `oldest_seq` → send `GAP{from: cursor, to: oldest_seq}`,
  then replay from `oldest_seq`. The client knows exactly how much it missed
  and can decide whether to ask the network for `chathistory`.
- cursor 0 → start from a stored checkpoint if one exists, else `GAP` then the
  whole ring.

Replay is streamed, not buffered whole: the bouncer walks the ring, writing
frames straight into the attachment's outbound buffer, and sets `MORE` while
there is more. A 2 MiB replay of one attachment is 0 allocs because the frames
are built in a reused stack buffer (`[16]byte` header + copy of the line body
into the outbound ring — still no allocation, the outbound ring is the arena).

### 6.2.1 Windows and per-client cursors (B2, current implementation)

The bouncer tracks cursors per client **per window**, where a window is the same
key the log uses: network + window name (`status` for the server window). Only
the last acknowledged `seq` per client/window is kept, in memory; nothing is
persisted. A client that is not attached has no cursor and starts at
`oldest_seq`. `PlayBuffer` is window-scoped: for each requested window it
replays from `max(oldest_seq, cursor+1)`. If the client's cursor has already
been dropped by the ring, a `GAP` marker is written first, then the surviving
lines. The marker is one plain IRC line so the POC's text client can render it:

```
:<server> GAP <network> <window> <lost_from> <lost_to>
```

`lost_from` is the first sequence after the client's cursor and `lost_to` is the
last sequence the ring dropped (`oldest_seq-1`), i.e. the exact range the client
will never see.

`Attach`/`Detach` state lives next to the cursors: a window can be detached
(delivery stops, the cursor freezes) or muted (delivery stops, replay still
possible). Detaching and re-attaching replays exactly what arrived while away.
`ClearBuffer <window>` hides that window's already-broadcast lines and advances
every client's cursor past them, so cleared lines are never replayed; it is a
no-op with a message for an unknown window.

**Honest limitation.** The rings are still one per network, not one per window.
Per-window cursors index that shared ring, which approximates a per-window split
without fragmenting it. A very busy window can therefore push a quiet window's
lines out of the shared ring even though the quiet window's client was caught
up; that client sees a `GAP` where true per-window rings would have retained the
lines. One ring per window is a separate, larger change.

### 6.3 Checkpoint (the trick that keeps the bouncer dumb)

Re-parsing 2 MiB of raw lines to rebuild a nicklist on every attach is the cost
that makes bouncers grow state. Solution: the **client** writes its own
snapshot back.

- `CHECKPOINT` stores one opaque blob per session (≤ 512 KiB), with the `seq` it
  covers. The bouncer does not look inside it. It is bytes in a slot.
- A fresh client sends `CHECKPOINT_GET`, receives the blob, restores its state,
  then `ATTACH` from `covers_seq` and applies the tail.
- Blobs are written every N lines (default 512) or on a clean detach, by
  whichever client is caught up. Last writer wins; a stale blob is harmless
  because its `seq` is compared against the ring.
- A blob older than `oldest_seq` is dropped and reported as absent.

This gives fast attach without the server understanding a single byte of IRC.

## 7. Limits: server-enforced ceilings

### 7.1 Defaults (all configurable per deployment, per user via authorisation §8)

| limit | default | cap |
|---|---|---|
| networks (upstream connections) per user | 8 | 32 |
| attachments (clients) per session | 4 | 16 |
| ring bytes per session | 2 MiB | 16 MiB |
| ring bytes per user | 16 MiB | 256 MiB |
| checkpoint bytes per session | 512 KiB | 512 KiB |
| client frame payload | 64 KiB | 64 KiB |
| network line payload | 8 KiB | 8 KiB |
| outbound ring per attachment | 64 KiB | 256 KiB |
| lines/s per client connection (token bucket) | 5, burst 20 | 20, burst 100 |
| lines/s per user | 20, burst 100 | 100, burst 400 |
| sessions per user | = networks | = networks |
| nickname, channel, realname length accepted in IRC commands | 64 | 64 |

### 7.2 Enforcement points

- **Connection**: TLS, `HELLO` version check, `AUTH` before any other frame.
- **Attachments**: the cap is checked in `ATTACH`, refused with `ERROR` code
  `0x0301`; the oldest idle attachment is *not* evicted (surprising eviction is
  worse than a refusal, and the client can see the refusal).
- **Sessions**: `MANAGE` op `ADD_NETWORK` counts existing sessions against the
  user's ceiling before opening a socket.
- **Ring**: writing never blocks; a full ring overwrites the oldest segment,
  which is a documented lossy behaviour, not an error.
- **Outbound**: a slow client is the only thing that can be dropped. Per
  attachment ring; when it would overflow, the attachment is marked lagging,
  its outbound ring is flushed, and a `GAP` is queued so the client resyncs
  rather than receiving a half-written stream.

### 7.3 Refusal codes

| range | meaning |
|---|---|
| 0x01xx | protocol: bad version, bad frame, unknown type |
| 0x02xx | auth: 0x0201 token invalid, 0x0202 token expired, 0x0203 ticket invalid, 0x0204 no `bnc.manage` scope |
| 0x0301.. | limits: attachments, networks, ring bytes, rate, frame size |
| 0x04xx | management: unknown op, unknown session, not permitted |
| 0x05xx | upstream: 0x0501 connect failed, 0x0502 TLS failed, 0x0503 registration failed, 0x0504 SASL failed |

Every refusal names the limit it hit in the `ERROR` text. A client that is told
"attachments 4/4" can act; one that is told "denied" cannot.

## 8. OIDC, first class

### 8.1 The bouncer is a resource server, not a client

- Config: `oidc.issuer` (URL, discovery via
  `/.well-known/openid-configuration`), `oidc.audience` (the bouncer's own
  identifier), `oidc.allowed_algs` (default `ES256`, `RS256`).
- `AUTH` carries an access token. Validation: signature against JWKS, `iss`
  exact match, `aud` contains our audience, `exp`/`nbf` with 60 s skew.
  `none` and HMAC algs are refused by configuration, not by a runtime check.
- JWKS is cached; an unknown `kid` triggers one refresh, floored at one per
  five minutes. A failed refresh keeps the old keys until they would expire.
- `sub` is the user identity. Session ownership is `sub`, not a name, not an
  email.
- Token passthrough is not accepted: a token minted for another service is
  refused by the `aud` check. (RFC 8707 audience discipline.)

### 8.2 Tickets: decoupling the session from the token

Access tokens last minutes; IRC sessions last months. So `AUTH_OK` returns an
opaque **ticket**:

- 32 random bytes, base64url, bound to `sub`, TTL 24 h, renewable.
- Presented in `ATTACH` instead of the bearer token.
- A fresh access token renews the ticket without touching the IRC sockets — a
  token expiry never disconnects a network.
- Revocation: deleting the ticket kills attachments, not sessions. A session is
  killed only by `MANAGE REMOVE_NETWORK` or the limit reaper.

### 8.3 Client login flows

- `salotto login` — authorization code + PKCE with a loopback redirect on
  `127.0.0.1:0` (RFC 8252). Default for anything with a browser.
- `salotto login --device` — device authorization grant, for a headless box.
- `salotto login --token` — paste an access token, escape hatch for CI.
- Token cache: `$XDG_STATE_HOME/salotto/token`, mode 0600, refresh token when
  the issuer gives one.

### 8.4 Claims → limits

Precedence, highest first:

1. Per-user override file on the bouncer (`users/<sub-hash>.json`) — the
   deployment owner's word.
2. A `bnc_limits` claim, if present: `{"networks":4,"ring_kib":1024,...}`,
   clamped to the caps in §7.1. A claim can only lower, never raise past caps.
3. The deployment default.

### 8.5 Authorisation for management

- Attaching, reading, sending: any valid ticket for `sub`.
- `MANAGE` (add/remove networks, change per-network options, set limits):
  requires scope `bnc.manage` in the access token, or membership of
  `oidc.admin_group` for cross-user operations.
- A read-only client is a first-class thing: a token with no `bnc.manage`
  attaches and sends, and any `MANAGE` gets `0x0204`.

## 9. Session lifecycle and the network side

- A session = one user + one network + one socket + one ring.
- Connect: TCP → TLS (verify by default; `insecure_skip_verify` allowed per
  network with a warning in `STATUS`) → `CAP LS 302` → request
  `message-tags server-time batch echo-message away-notify account-notify
  extended-join cap-notify sasl chathistory` when offered → `SASL` if the
  network has credentials → `NICK`/`USER` → done.
- The bouncer **answers `PING` itself** and does not forward it to clients.
  That is the only network→client line it produces on its own, and the only
  line of IRC it parses on that path (verb + token).
- Everything else is forwarded verbatim with a seq number.
- Reconnect: capped exponential backoff (1 s → 60 s, ±20 % jitter), unlimited
  attempts, `STATUS` publishes the state (`connecting`, `registered`, `backoff`,
  `failed`) and the ring gets a synthetic one-line `NOTICE` from the bouncer
  so the client can render "reconnecting" in-band, in the right place in
  history.
- On reconnect the bouncer re-joins nothing. The client knows its channels and
  sends the `JOIN`s after `registered` — because the bouncer does not know what
  a channel is (§1). Rejoin-on-reconnect is a client feature, driven by a
  `STATUS` transition.

### 9.1 The one exception to dumbness

A channel cap per network is a stated requirement, and counting channels needs
to see `JOIN`/`PART`. So the client→network path gets exactly one piece of
parsing: the **first token** of a line, compared with `bytes.EqualFold` against
`JOIN` and `PART`, to maintain a counter. The network→client path stays
untouched. This is written down as an exception because it is the kind of thing
that becomes ten exceptions if nobody stops it: no channel cap, or this
paragraph. Recommendation: take the paragraph.

## 10. The client

- TUI first (`salotto`), one process, one connection to the bouncer per
  session or one multiplexed connection to many sessions.
- State: parsed channels, nicklists, topics, modes, unread marks, scrollback —
  all in the client, rebuilt from checkpoint + replay.
- Writes a `CHECKPOINT` every 512 lines while caught up.
- Keeps its cursor in `$XDG_STATE_HOME/salotto/cursors` (per session), so a
  restart is a resume, not a replay from zero.
- Local echo: when the session negotiated `echo-message`, the client renders
  nothing until the server echoes (its `SEND_OK` gives it the `client_line_id`
  to correlate). When not, it renders optimistically and marks the line local
  (`LOCAL_ECHO` flag on the synthetic entry) so the history stays honest.
- A second client attached to the same session sees the same lines from its own
  cursor. Sends from one client appear in the other's stream as the network
  echoes them — no cross-client messaging path, no duplicated state.

## 11. Backfill beyond the ring

The ring is finite and lossy by design. For older history, the client asks the
network, not the bouncer:

- If the session negotiated `chathistory` (IRCv3), the client issues
  `CHATHISTORY BEFORE ...` and renders the result like any other line.
- Otherwise: no backfill. The `GAP` frame tells the client how much it lost,
  and that is the honest answer.

## 12. Repo layout

```
cmd/salottod/          bouncer main
cmd/salotto/           client main
internal/frame/        header, encode/decode, no alloc
internal/arena/        segment ring, record walk, no alloc
internal/session/      socket, registration, PING, backoff
internal/limits/       token buckets, counters, ceilings
internal/oidc/         discovery, JWKS cache, claim mapping
internal/manage/       MANAGE ops, per-user config
internal/client/       attach/resume/replay/checkpoint
internal/tui/          client UI
testdata/transcripts/  recorded IRC sessions, replayed into the fake server
docs/SPEC.md           this file
```

## 13. Transport adapters (later)

Same framing, different carrier:

- TLS 1.3 + raw TCP (default).
- Binary frames over WebSocket for a browser client, `Sec-WebSocket-Protocol:
  salotto.v1`. `ATTACH` and replay are identical; only the byte carrier moves.
- Optional Unix socket for a client on the same host, no TLS.

## 14. Deliberate omissions

- No bouncer-side push, no e-mail, no webhooks.
- Server-side log search over the daily index (§2.1), loopback by default.
- No file transfer beyond what the network carries.
- No plugin system. A bouncer that loads plugins is a bouncer that grows state.
- No persistence by default: a `salottod` restart loses the rings. Optional
  `--state-dir` with the rings mmapped so a restart reattaches them
  (milestone M5, off by default).

## 15. Testing and the alloc gate

- `internal/frame` and `internal/arena`: table tests plus a fuzz target on the
  decoder that asserts no panic and no allocation on valid input.
- `BenchmarkRingAppend`, `BenchmarkReplay1MiB`, `BenchmarkFrameParse`,
  `BenchmarkSendPath` — each must report `0 B/op, 0 allocs/op`.
- CI gate: parse `-bench` output, fail on any non-zero `allocs/op` in those
  four. Plus a `testing.AllocsPerRun` assertion in the unit tests so a
  regression fails locally too, before the benchmark noise.
- A fake IRC server that replays `testdata/transcripts/*.irc` at a controlled
  rate; `-race` on everything.
- End-to-end: one bouncer, two clients, one session; kill a client, attach a
  third, verify the `GAP` maths and the checkpoint path.
- OIDC: a local test issuer (a 40-line httptest server signing ES256 with a
  generated key, serving discovery + JWKS), cases for wrong audience, expired
  token, unknown kid, missing scope, claim-based limit clamp.

## 16. Milestones

| | deliverable | proves |
|---|---|---|
| M0 | `internal/frame` + `internal/arena`, benchmarks green at 0 allocs | the whole premise |
| M1 | one session, one client, send + replay, fake server | the dumb core |
| M2 | OIDC, tickets, per-user sessions, limits | multi-user |
| M3 | TUI client with checkpoint + cursor + local echo | daily driver |
| M4 | real networks: TLS, CAP, SASL, backoff, chathistory | usable |
| M5 | optional mmapped rings, WebSocket adapter | polish |

## 17. Open decisions

1. **Multiplexing.** One TCP connection per session (simplest, one attachment
   each) versus one connection carrying many sessions. Recommendation: one
   connection per session in v1; the frame already carries a session id, so
   multiplexing later is an adapter, not a redesign.
2. **Checkpoint authorship.** Client-written (this spec) versus bouncer-written
   from a client-declared grammar. Recommendation: client-written; anything
   else drags IRC knowledge into the bouncer.
3. **Channel cap.** Accept §9.1's single parse, or drop the cap. Recommendation:
   accept it, and keep it to exactly `JOIN`/`PART`.
4. **Unread markers.** Per-client (in the client) or shared per user in the
   bouncer. Recommendation: per-client, stored with the checkpoint. Shared
   markers need state the bouncer promised not to keep.
5. **Multi-user group bouncer.** Several humans sharing one `salottod`. The
   model already allows it (`sub` isolation); the question is whether limits
   should be per deployment or against a shared quota. Recommendation: v1
   per-user only, no shared pool.
6. **Licence.** Nothing is chosen, and nothing here should be published or
   accepted as a contribution until it is. Recommendation: Apache-2.0, matching
   Jitsi Meet and matching the notice discipline stanze already uses, or MIT if
   the intent is "take it and do what you want". Either way add `LICENSE` and a
   `DCO`/`CLA` line to `CONTRIBUTING.md` before the first outside patch.
