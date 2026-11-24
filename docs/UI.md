# The salotto UI: one model, three renderers

This is the design QUEUE.md §2.0 asks for, implemented. The rule that makes it
hold is one sentence: **the UI is a `tea.Model` and nothing in it knows what it
is drawing into.**

## Why it is shaped this way

The tempting shape is a TUI plus "the same thing in HTML", which is two UIs and
two sets of bugs. The shape here is a value:

- input is a `tea.Msg` (`ui.Incoming`, `ui.Sent`, `tea.KeyMsg`, `tea.WindowSizeMsg`);
- output is one string of plain text plus ANSI (`Model.View()`);
- nothing in `internal/ui` dials, listens, reads a clock, or touches a terminal.

So the browser does not get a second UI implementation. It gets **the terminal
rendering**, converted. That makes monospace free (it *is* a terminal grid), the
charmbracelet look free (it *is* lipgloss), and leaves exactly one CSS file,
buildkite's `terminal.css`, embedded in the binary.

## The three drivers

| front end | input | output | code |
|---|---|---|---|
| SSH TUI | a real terminal over SSH | terminal | `cmd/salotto-ssh`: `wish` for SSH, a `tea.Program` we own for the terminal |
| Web | browser → HTTP → HTML | HTML | `internal/ui/web.go`, `terminal-to-html/v3` |
| headless (tests, bots) | synthetic `tea.Msg` | string compare | `ui.Headless` |

All three hold the same `ui.Model`, wrapped by `ui.Program` where a front end also
needs a bouncer connection. `ui.Program` owns the model and the transport and is
the only place the two meet; the model itself still knows nothing about sockets,
which is what keeps the second renderer from existing.

Why `salotto-ssh` builds its own `tea.Program` instead of using wish's bubbletea
middleware: the middleware constructs the Program internally, so a goroutine
holding a transport has no way to `Send` into it, and a front end that cannot be
fed lines is a blank screen. Four lines of Program setup buys an obvious data
flow — wish handles SSH, bubbletea handles the terminal, `main.go` is where they
meet.

All three hold the same `ui.Model`. `TestOneModelThreeDrivers` asserts that the
headless driver's `View()` is byte-identical to the direct one, and that the
value satisfies `tea.Model` (which is what `tea.NewProgram` requires).
`TestWebMatchesHeadlessView` asserts the served page embeds the conversion of
that very string. If a second renderer ever appears, those two tests fail.

`Model.Update` returns `tea.Model` because that is the interface bubbletea wants;
`Model.Step` is the same function returning the concrete type, which is what the
web and headless drivers loop on without an assertion per message.

## Two traps, both real, both pinned by tests

1. **24-bit colour is silently dropped.** `terminal-to-html` v3 maps 8-bit SGR
   (`38;5;n`) to `term-fgx*` classes; its 24-bit branch is an empty `case`. A
   truecolor lipgloss style therefore reaches the browser as unstyled text, with
   no error anywhere. Every colour in `internal/ui` is a 256-palette index and
   `Configure()` pins `termenv.ANSI256`; `TestViewIsStyled` fails on any
   `38;2;`, and `TestNoTruecolorSurvivesHTML` fails if SGR leaks into the page.
2. **A daemon has no TTY.** Without an explicit profile, lipgloss resolves the
   ASCII profile and strips all colour *before* the converter sees it. Both
   binaries call `ui.Configure()` first, and the SSH server also pins the
   session renderer. The test binary calls it in `TestMain` for the same reason:
   without it the styling assertions would silently pass on colourless output.

Two smaller ones found while building:

- `html/template` escapes the converter's `<span>`s unless the value is
  `template.HTML`. Serving the converted string as a plain `string` produced a
  page full of `&lt;span&gt;`, which `TestWebMatchesHeadlessView` catches.
- `bubbletea` resolves Ctrl-N/Ctrl-P to their own `KeyType`s, not to `KeyRunes`,
  so window switching must be handled as `tea.KeyCtrlN`/`tea.KeyCtrlP`.
  `TestWindowSwitching` covers it, including the one-window no-op.

## Running them

```
go build -o salotto-ssh ./cmd/salotto-ssh
./salotto-ssh -addr :2222 -host-key ~/.ssh/salotto_ed25519
ssh -p 2222 localhost            # the TUI, in any terminal

go build -o salotto-web ./cmd/salotto-web
./salotto-web -addr 127.0.0.1:8080
curl 'http://127.0.0.1:8080/?width=70&height=16'
```

The `?width=`/`?height=` parameters exist so the page can be exercised without a
browser; they are applied through the same `tea.WindowSizeMsg` the terminal
driver sends, not a parallel path (`TestWebResizeIsTheSameWindowSizeMsg`).

## The window list: grouped by server, numbered like weechat

The sidebar is the one part of the screen that needs a fact the model is not
given by the IRC protocol: which network a window belongs to. A live line is
not inside a `BATCH`, so the batch id replay happens to carry is not available,
and two networks can both hold a window called `status`. The bouncer therefore
writes `@salotto.network=<net>` on **every** line it produces, live and
replayed (`internal/ircline.TagNetwork` spells it for both ends), and the client
learns the network from the line rather than guessing it from a batch.

`Model.netOf` caches window → network, fed by lines and by `353` membership -
membership, because a quiet channel's first traffic is usually its JOIN and a
sidebar that waited for a message would file the channel under nothing for
hours. The map is never cleared by an untagged line: a relayed line or a web
frame rebuilt from JSON must not move a window out from under its heading.

`sidebarRows` builds the list once and both the renderer and `WindowAt` read it.
That is deliberate - the two used to walk `m.order` separately, and the first
heading makes an index off by one, which is a click on `#salotto` selecting the
row above it. A heading row has no window, so a click on one does nothing; and
when the headings do not fit the budget the list falls back to flat rather than
clipping, because half a grouped list is worse than the flat one it replaced.

Buffer numbers are identity, not position: assigned the first time the model
sees a window, never reassigned, never reused after a close. `/b <n>` and
`/buffer <n>` switch by number, bare `/b` lists them, and `/b` lives in
`modelCommands` so `/help` and Tab derive it instead of a hand-written string.
Both behaviours are `/fset` settings (`group_by_server`, `buffer_numbers`),
defaulting to on; `group_by_server=off` is the flat arrival-order list, drawn by
the same renderer.

Window *identity* carries the network: `windowKey(network, name)` is `"network/name"` when the
network is known and the bare name when it is not, and every map the model keeps about a window
is keyed by it. So two networks' `status` are two windows, with two numbers and two scrollbacks
(QUEUE.md §2.1.1). The key is internal: the title row, the sidebar, the buffer list and search
hits render `winName`, and `CHATHISTORY` sends the bare name because the bouncer resolves the
network itself. A window whose network is learnt late is rekeyed by `migrateKey`, which moves
every map at once and keeps the buffer number the user has already learnt. When two networks
hold the same name the buffer list prints `status (libera)`, and that spelling is a valid `/go`
target, so the list never names a window nobody can type back in.

## Reconnecting without doubling the scrollback

The transport owns the retry loop, not the drivers. Three front ends, one of them spawning a
process per browser tab, would each have to write the same backoff and get the same details
right; `Transport.run` does it once: 1 s doubling to 30 s, stopped only by an explicit `Close`,
which is QUEUE Part 3 item 12's rule ("the only stop is an explicit user action"). `Dial` still
fails to the caller on the first attempt, so a wrong address stays distinguishable from a
bouncer that restarted.

`Msgs()` therefore means *this client is finished*, not *this attempt failed*. A closed channel
is what the pump and the drivers already understood; a reconnect they never hear about is a
reconnect they cannot break.

The reconnect repeats the handshake from the credentials kept at the first `Register`, and the
model hears three things: `Disconnected` (the reason), `Reconnecting` (once per outage, because
a transport retrying every two seconds would otherwise fill the status window with one notice
per attempt), and `ReconnectFailed` (once a minute - a refused socket is the message that
suggests the bouncer is not running at all, and it needs saying without becoming noise).

The part that is easy to get wrong is the replay. A bouncer replays its whole ring on
registration, so a naive reconnect re-receives everything the client already holds and the
scrollback grows by the size of the ring on every reconnect - measured: 3 lines in, 3 replayed,
6 on screen.

The marker is the bouncer's own tag: a replayed line arrives inside a `BATCH +/-replay` group and
carries `batch=<id>`, and no live line does. That is a fact about the line, which is the only
kind of fact that survives here. Two earlier versions of this were wrong in ways worth recording:

- **Ordering is not the marker.** The first version turned suppression on at a reconnect and off
  at 376, assuming the registration burst ends after the replay. A probe against a built daemon
  read `376` at line 8 and the first `BATCH +replay` at line 9, so the window was shut before a
  single replayed line arrived - deduplication was dead code in production and green in tests
  that placed `ReplayFinished` wherever they liked.
- **The timestamp is not the marker.** A live line's `time` tag is stamped when the bouncer fans
  it out; a replayed line's comes out of the ring where it was stamped when buffered. Two clock
  reads: `17.242` live against `17.241` replayed for the *same message*. A time-based comparison
  fails for every line, invisibly, whenever the fixture reuses one timestamp.

Identity within a batch is nick, verb and text in one window, and it is a multiset: the first
`baseline` copies a batch delivers are the ones already on screen, and anything past that count is
news the client missed while it was away. The baseline is fixed when the batch starts and not
recomputed per line - recomputing counts the copies the batch itself has drawn, which measured as
"a batch of three over a window holding one left two". The counter is per batch, so a second
replay reconciles from scratch instead of drawing one extra copy of everything.

A live line is never compared, because the bouncer never tags one: an untagged repeat is a server
saying the same thing twice, and dropping it is the silent loss this package refuses. The search
covers the last 512 lines of a window, which is bounded work per line and still correct, because a
replay cannot be longer than the ring.

## Settings you can edit where you read them

The settings screen is a mode over the conversation, and for a while it was a read-only summary:
booleans toggled, `scroll_step` stepped, and every other row said "edit with `/fset`". That is
honest and it teaches the wrong thing - a user who opened the screen to change a highlight
concluded the screen could not do it and that settings lived in a command they had to remember.

A text row now opens an editor in place. Enter commits, escape abandons, and while it is open the
row shows what is being typed with a caret where the value lives. The keystroke handling is a
separate branch for exactly one reason: a space in the editor is a space in the value, which is
the bug the old behaviour had - pressing space on a rule list called `set(key, "")`, and with the
blob shared at the bouncer that wiped the list on every client the user owned. Arrows do nothing
while typing rather than jumping rows, because a jump would redirect the rest of the typing into a
different setting.

Adding was the only operation the screen had, so a list could only grow: the single removal was
`""` / `none` / `clear`, the whole list. A value with a leading `-` now removes one word
(`-alice`), answers when the word was not there, and `none` still clears everything - two
different statements, deliberately still spelled differently. Both go through `Settings.set`, so
the screen and `/fset` cannot drift into accepting different spellings of the same thing.

## What is deliberately not here yet

- **No WebSocket.** W1 replaces the request-per-page part; the render half in
  `web.go` does not change, which is the point of the split.
- **No OIDC, no tickets.** The web front end is loopback-only until `docs/SPEC.md`
  §8 lands; it has no authentication of its own and must not be exposed.
- **No PWA shell.** §2.0.1 needs the WebSocket first, because a cached shell that
  cannot reconnect is worse than no shell.
- **No `salotto.v1` frames yet.** `internal/ui/transport.go` speaks plain IRC to a
  bouncer today and produces `ui.Incoming` values. The seam is the right one:
  `Incoming` is a plain struct, so a protobuf decoder can build the same values
  without the model changing. `docs/UI-E2E.md` covers how the wiring is proven
  against a deployed bouncer.
