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
saying the same thing twice, and dropping it is the silent loss this package refuses.

The baseline is a tally of the window taken once, on the batch's first line, not a search per line.
That distinction is the whole of it: an earlier version scanned the last 512 lines for each replayed
line, which made a long reconnect quadratic and, worse, was wrong - the daemon replays a window's
*whole ring*, so after a real absence the first lines of the replay matched nothing inside the cap
and were drawn again. Measured against the old code, a 2000-line window replayed after a reconnect
came back as 4000, and a batch of 700 identical lines over a window holding none delivered 699,
because a capped scan loses as well as repeats. Tallying once makes the per-line work a map lookup,
so there is no cap to get wrong.

## The settings file is written when a setting changes

Two defects lived in the gap between "the model holds the right value" and "the file holds it",
and neither is visible from a test that only reads the model.

`saveSettings` appended its failure message to its own copy and returned nothing, because it was a
value receiver. A full disk or a read-only home therefore meant every setting change silently did
not persist, and the symptom the user reported would have been "my settings keep resetting" -
investigated in the sync code, while the cause was local. It now returns the model with the failure
drawn, and the change still applies in memory: reporting a write failure must not undo what the user
asked for.

The remembered window was written to disk on every window switch, and a switch happens on every
step of a search walk. Holding the down arrow in a result list meant one whole-document write per
keystroke, for a value that is only read at startup. The write now happens with the next real
settings change, which is the same granularity the feature promises - "my settings sync", not "my
scrolling syncs".

A third defect surfaced while testing the second: `cycle` (`^N`/`^P`) assigned `m.active` directly
instead of going through `SetActive`, so every keyboard window switch was invisible to the
remembered position. A user who moved around with the keyboard and came back landed wherever they
had last clicked.

## A search result is something you can go to

`/search` used to write its hits into the active window as a numbered list of text. That answered
"did I spell it right" and stopped: the line a user was looking for was in another window, possibly
a thousand lines back, and the list gave them a name and no way to reach it.

The hits are now a list you walk. `/search <text>` opens it in place of the conversation, the
arrows move the selection, `enter` commits, `escape` leaves. Moving the selection already moves the
view, so a hit is read in its context before it is committed to - a picker that only shows text and
jumps on enter asks the user to choose blind.

Three decisions in that:

- **The list is a mode, not a window.** A search is a question about the windows, not a place the
  conversation happened; giving it a buffer would put it in the sidebar, in the unread counts, in
  `/b`, and in the replay, none of which it belongs in.
- **`escape` does not undo the walk.** The user already found what they were looking for; dragging
  them back to where they started would hide it again. Leaving the list stays wherever the last hit
  was.
- **Any other keystroke closes the list and goes to the input.** A mode that swallows typing traps
  the user in it, and the first thing anyone does after finding a line is say something about it.

The jump scrolls so the hit sits a few rows below the top rather than exactly at it: a match flush
against the frame edge reads as the first line of the buffer, and the point of arriving is to read
around it. A hit is placed by its whole identity - nick, text and kind - because two people can say
the same words, and finding it by text alone lands on whichever copy is nearer the bottom.

Search still covers only what this client holds in memory. Searching the bouncer's whole ring is
§2.1 W4 and is a different feature: it needs the daemon to answer, and it is not what "jump to
result context" was.

## The window you were in is a setting

A refresh should not land you on `status`. The window a client is looking at travels in the
settings blob as `last_window`, a window key, so the bouncer carries it the way it carries every
other setting - opaquely, fanning it out and persisting it - and no second protocol is needed for
one string.

Three rules make it safe with more than one client attached:

- **Only a client that has not chosen a window adopts it.** A fresh client opens on its default
  window, which is an artefact of startup. If being restored counted as a choice, a second tab or a
  phone connecting would write its default over where the user actually was, and "resume where you
  left off" would become "resume wherever the last thing to connect happened to default to".
- **A client already somewhere is not dragged back.** Two clients attached at once disagree about
  the active window by definition; moving the other one would yank the tab the user is reading.
- **An empty value is an opinion too**, and it means the bouncer holds none: the client stays
  where it is rather than guessing.

The stored value is a key, so it carries its network and a window on a second network restores into
the right group. A key this client has never seen a line in is created rather than refused - the
blob and the replay arrive in either order, and the lines are about to land in it.

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
