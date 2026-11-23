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
| SSH TUI | a real terminal over SSH | terminal | `cmd/salotto-ssh`, `wish` + `wish/bubbletea` |
| Web | browser → HTTP → HTML | HTML | `internal/ui/web.go`, `terminal-to-html/v3` |
| headless (tests, bots) | synthetic `tea.Msg` | string compare | `ui.Headless` |

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
