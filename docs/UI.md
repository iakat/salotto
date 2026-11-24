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

## A client opens on `status`, not on its own nick

ZNC names a client's server window after the user, and the TUI copied that: with no `-window` it
opened on a buffer called after the nick. This bouncer does not work that way - every line carries
the window it belongs to, and that window is `status` - so the client created a buffer nothing could
ever write to while the daemon's real status lines went into a second one. The sidebar showed
`1 nicholas` (permanently empty) and `2 status` (holding everything), and the empty one had a number,
an entry and no way to be filled.

The rule is `ui.StartWindow`, in the same package as the model so it can be tested: a rule that
lives in a `main` package is a rule pinned only by whatever happens to run the binary. `-window`
still overrides, because starting in a channel deliberately is a real thing to want; the rule is
only about the default.

The general shape, which is worth keeping: **a window that can never receive a line should never be
created.** The identity rules already prevent two windows for one name; they cannot prevent a front
end from opening on a name nothing addresses.

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

### The list now asks the bouncer, not only the window in memory

The picker above was still answering the small question. Its hits came from `Model.Search`, a
linear scan of the scrollback this connection holds, so on a bouncer that had been running for
weeks a line from last month was unfindable even though it was on disk - the client was searching
the last few hundred lines of each window and calling that "search". The bouncer's log index
already existed (`logfile.Query`, the same walk `CHATHISTORY` uses); nothing on the wire asked it.

`/search` now sends a `SEARCH` verb to the bouncer and the bouncer answers from the index:

```
SEARCH [id:<n>] [window:<name>] [network:<name>] [since:<YYYY-MM-DD>] [until:<YYYY-MM-DD>] <query>
NOTICE <nick> :SEARCH <date> <time> <network> <window> <body>
NOTICE <nick> :SEARCH-END <count>
NOTICE <nick> :SEARCH-ERR <reason>
```

The body is last and unescaped. It is the only free-text field in the format, and the reason the
row is not, say, tab-separated is that a log body may contain tabs, colons and runs of spaces: a
reader that splits the first four fields and takes the rest verbatim cannot lose any of them,
while an escape scheme would have to be invented and agreed on by two ends for no benefit.

Four decisions in that:

- **Logging off is a refusal, not an empty answer.** With no log directory `SEARCH-ERR logging
  disabled` is written, because `SEARCH-END 0` is indistinguishable from "nothing matched" and
  sends the user looking for a spelling mistake in a search that never ran. The same rule applies
  to a path in `window:` or `network:`: it is refused at the boundary with the field named, rather
  than passed to the filesystem walk.
- **The client does not wait.** The disk is the daemon's to scan; the model returns a command,
  draws a `searching for "…"` row where the results will appear, and keeps drawing. A search that
  blocked the render loop would freeze the input line behind a file walk, and on a busy bouncer
  that is exactly when the user is most likely to be typing.
- **A superseded answer is dropped, not shown late.** Each request carries an `id:` and every
  reply line repeats it as the `salotto.search` tag. An answer whose id is not the newest is
  discarded. A late picker is worse than no picker: it looks like the answer to the question on
  screen, so the user walks a list from a query they have already replaced and only notices when
  the rows are about words they no longer remember typing. The pending row remains, which is the
  honest state - the current question has not been answered.
- **One predicate chooses the path.** `searchRemote` is set when the registration burst's `002`
  says salotto and cleared when the connection drops. With it, `/search` is asynchronous and asks
  the index; without it, the old synchronous in-memory scan runs unchanged, which is what a
  headless renderer, a test and a plain IRCd client all still get. Falling back to memory after a
  failed remote search was considered and rejected: that is a second decision in a second place,
  and the two would eventually disagree about which history was searched.

**Fixed**: `/search` crosses the restart and reaches every window the bouncer has logged, not only
the ones this connection happens to hold. **Made visible, not fixed**: the default range is the
bouncer's own - today and the previous seven days, at most 200 rows - so a search further back
needs `since:` (the cap is Query's and is not raised here). And a hit that exists only on disk is
not in the client's ring, so `enter` opens the right window but lands at its bottom; the row still
shows the date and time so the line can be found there. Streaming a hit into the ring to scroll to
it would be a second history path, and a client that silently manufactured a line is worse than one
that says where it is.

The jump scrolls so the hit sits a few rows below the top rather than exactly at it: a match flush
against the frame edge reads as the first line of the buffer, and the point of arriving is to read
around it. A hit is placed by its whole identity - nick, text and kind - because two people can say
the same words, and finding it by text alone lands on whichever copy is nearer the bottom.

The list is still a question about history, not a second buffer. A disk hit is
not appended to the client's scrollback just so the jump can scroll to it: the row says when it
happened and the jump opens the window, which is the part the user acts on next.

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

## A command is not a message, but a message can be spelled as a command

Two bugs with one cause, both found by driving the real binary rather than by a test.

The first: every submitted line that went to the wire was echoed into the window as a line of
ours. `/topic a new topic` therefore appeared as the literal text `/topic a new topic`, and a
moment later the server's confirmation appeared beside it - the command drawn as if it were
conversation, and the effect reported twice. What a command does is the server's to report; the
client has no business showing you your own syntax.

The second, found while fixing the first: `/msg alice are you there` drew *nothing at all*. No
query window opened, no line appeared. The line went out and the only evidence was the relayed
copy arriving later, in a window nobody had opened. Silence after pressing enter is the worst of
the available failures - worse than a duplicate, because a duplicate at least tells you what the
client thinks it did.

So the rule is not "commands are not echoed". It is:

- **A command that changes state is not drawn.** `/topic`, `/mode`, `/kick`: the server reports
  the effect itself, and drawing the instruction too means seeing it twice.
- **A command that carries a body is drawn, as its body, in the window it is addressed to.** `/me`
  is a CTCP ACTION; `/msg` and `/query` open the window of the person being addressed and switch
  to it, because the next bare line belongs to that person; `/msg #chan hi` draws `hi` in the
  channel, not the command. What separates them from `/topic` is that they have an addressee and
  a body, not that they start with a slash.
- **`/notice` follows the message rule but does not open a window.** A notice addressed to a
  person is a system line, and a query window containing one timestamp and an OTR handshake is
  the annoyance ZNC users complain about.
- **`*status` is never a query.** `/msg *status help` is a request to the bouncer, and
  retargeting it would both open a bogus window and eat the answer.

`carriesBody` and `retargetWindow` in `internal/ui/model.go` are those last two rules; the echo
itself still goes through `echoParts`, so a long `/msg` is drawn as the marked parts it will
become.

The second bug also existed one layer down: the transport filed a private message under its
*sender*, so our own PM to alice - relayed back by the server, as every server does - landed in a
window named after ourselves. It is filed under the other party now: when the sender is us, the
window is the target.

## A whois answer is one block, not eight numerics

`/whois alice` used to draw the server's numerics raw into `status`:

```
311 nicholas alice ~a h * :Alice Smith
319 nicholas alice :#salotto #dev
312 nicholas alice irc.libera.chat :Libera Chat
330 nicholas alice alicesmith :is logged in as
317 nicholas alice 7300 1700000000 :seconds idle, signon time
318 nicholas alice :End of /WHOIS list
```

That is not an answer to "who is alice". The numbers are the server's names for fields, the colons
are separators the wire needs and the reader does not, `7300` makes the reader divide by 600, and
the last line exists only because the reply had no other way to say it was finished.

Now the family is collected and drawn once, when `318` closes the reply:

```
alice
  ~a@host.example (Alice Smith)
  on #salotto #dev
  on irc.libera.chat (Libera Chat)
  logged in as alicesmith
  idle 2h1m, signed on Tue 14 Nov 22:13
```

Four decisions hold it up:

- **The answer goes where the question was asked.** Typed in `#salotto`, it is drawn in `#salotto`.
  The bouncer does not know where the user was looking, so the transport remembers it at send time
  (`noteWhois`) - and remembers it *then*, because by the time the reply arrives the user may have
  moved, and the answer belongs where the question was, not where they wandered. A reply to a
  question this client never asked has no such window and goes to `status`.
- **An unknown numeric in the family is still drawn raw.** A raw line is worse than a good one and
  much better than silence; the family is a list of verbs, and anything outside it falls through to
  the old path.
- **Two interleaved whois replies stay separate** because the answers are collected per subject,
  keyed case-insensitively: the wire does not promise to finish one before starting the next.
- **Idle time is said in hours.** `7300` seconds is a number; `2h1m` is an answer.

The collection is a `map[nick]*whoisPending` on the transport, deleted when `318` draws it - state
that exists for the length of one exchange, not a second store of anything the bouncer owns.

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
