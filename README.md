# salotto

A replacement for The Lounge: one Go bouncer that holds the IRC connections,
one client that runs anywhere.

The bouncer is deliberately dumb. It keeps sockets, a replay ring and a cursor
per client, and it does not know what a channel, a nick, a topic or a mode is.
Everything that needs to understand IRC happens in the client.

```
  client A ─TLS──┐
  client B ─TLS──┼──▶ salottod ──TLS──▶ irc.libera.chat
  client C ─TLS──┘        │         ──TLS──▶ irc.ircnet.org
                          │
                    ring per session,
                    cursor per client
```

## What it promises

- **Presence.** Network sockets live in the bouncer. Clients attach, detach,
  crash and come back on another machine; the network never sees a disconnect.
- **A dumb bouncer.** Raw lines are framed, numbered, kept in a bounded ring
  and replayed from a cursor. No IRC state on the server, ever — not as a
  cache, not as an optimisation.
- **0 allocations in the steady state.** Line → ring → client, replay, and the
  send path are measured at `0 B/op, 0 allocs/op`, and the build fails if that
  regresses. The exact scope of the claim, and the rules that make it hold,
  are in §3 of the spec.
- **OIDC first class.** The bouncer is an OAuth2 resource server: JWKS
  discovery, audience checks, tickets that outlive an access token, limits that
  come from claims and can only lower the deployment ceiling.
- **The client manages the bouncer within limits.** Add and remove networks,
  change per-network options, raise or lower its own quotas up to the cap. The
  server enforces the ceiling and names it in every refusal.

## Read the spec first

[`docs/SPEC.md`](docs/SPEC.md) — framing, the ring and cursor model, the
checkpoint trick that keeps the bouncer IRC-blind, limits, OIDC, the client,
the alloc gate, milestones, and the open decisions that still want a call.

Two decisions are worth knowing before reading anything else:

- **Checkpoints are written by the client.** A fresh attach restores a snapshot
  the client itself produced, then replays the tail from the ring. The bouncer
  stores the blob and never looks inside it.
- **There is exactly one place the bouncer parses a line**: the first token of
  a client→network line, compared against `JOIN` and `PART`, to count channels
  for a per-network cap. §9.1 says why, and argues for keeping it to exactly
  that.

## Status

Spec draft, nothing implemented. The milestone table in the spec starts with
the frame codec and the ring, because those two carry the whole premise.

## Layout

```
cmd/salottod/          bouncer
cmd/salotto/           client
internal/frame/        header, encode/decode, no alloc
internal/arena/        segment ring, record walk, no alloc
internal/session/      socket, registration, PING, backoff
internal/limits/       token buckets, counters, ceilings
internal/oidc/         discovery, JWKS cache, claim mapping
internal/manage/       MANAGE ops, per-user config
internal/client/       attach, resume, replay, checkpoint
internal/tui/          client UI
testdata/transcripts/  recorded IRC sessions replayed into the fake server
docs/SPEC.md           the spec
```

## Licence

Not chosen yet — see open decision 6 in `docs/SPEC.md`. It wants a call before
the first outside contribution, not after.
