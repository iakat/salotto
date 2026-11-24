package ui

import (
	"context"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
)

// Program wraps a model and a transport into something a front end can run: the
// SSH front end runs it as a tea.Program, the web bridge drives it from its own
// loop, and a test can drive it with no terminal at all.
//
// This is the file that was missing. The model and the renderers existed and were
// tested, but nothing connected either of them to a bouncer, so every front end
// faithfully drew an empty screen. A UI that renders nothing correctly is still a
// UI that shows nothing.
type Program struct {
	// mu guards model. The SSH front end drives Step from one goroutine and
	// needs no lock, but the web front end renders from HTTP handlers while a
	// pump goroutine feeds lines in, and handing a model to two goroutines
	// without a lock is how a chat client corrupts its own window list.
	mu    sync.Mutex
	model Model
	tr    *Transport

	// previews remembers link cards. It is a cache and not a map on the model
	// because it outlives any one window: the same link shown in two channels is
	// one fetch, which is the whole reason previews are done server-side.
	previews *PreviewCache

	// previewsOnce makes the cache lazy: a TUI session that never opens a browser
	// must not pay for a fetcher it cannot use.
	previewsOnce sync.Once

	// rev counts applied messages. A WebSocket driver needs to know when the
	// render changed without being told explicitly by every path that changes it;
	// a counter bumped in one place is that, and a missed frame is harmless
	// because every frame carries the full render.
	rev uint64
}

// Key applies one keystroke, named the way a terminal names it. The web client sends
// these instead of text so both front ends go through updateKey: the buffer, the
// history and the command mapping live in one place.
func (p *Program) Key(name, runes string) {
	var msg tea.KeyMsg
	switch name {
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "backspace":
		msg = tea.KeyMsg{Type: tea.KeyBackspace}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		msg = tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		msg = tea.KeyMsg{Type: tea.KeyDown}
	case "tab":
		msg = tea.KeyMsg{Type: tea.KeyTab}
	default:
		if runes == "" {
			return
		}
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(runes)}
	}
	p.Step(msg)
}

// Mouse applies a click at a character cell. The model decides what is there: it drew
// the screen, so it is the only thing that knows where the window list is.
func (p *Program) Mouse(x, y int) {
	p.Step(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: x, Y: y})
}

// Resize tells the model how big the client is. A driver calls it whenever the
// viewport changes; the model needs it because it renders a fixed number of lines and
// a width, and guessing either produces a window that either wraps or is short.
func (p *Program) Resize(width, height int) {
	if width < 20 {
		width = 20
	}
	if height < 6 {
		height = 6
	}
	p.mu.Lock()
	p.model, _ = p.model.Step(tea.WindowSizeMsg{Width: width, Height: height})
	p.rev++
	p.mu.Unlock()
}

// previewsPerWindow bounds how many cards one window is worth fetching for: a message
// with a dozen links does not need a dozen requests to strangers.
const previewsPerWindow = 3

// previewCache returns the preview cache, creating it on first use. Lazy because a
// TUI session that never opens a browser must not pay for a fetcher it cannot use.
func (p *Program) previewCache() *PreviewCache {
	p.previewsOnce.Do(func() {
		// Only if nothing was handed in: a caller that supplied a cache (a test with a
		// fake fetcher, or a future one shared between sessions) must not have it replaced
		// by a live one the first time a preview is primed.
		if p.previews == nil {
			p.previews = NewPreviewCache(NewFetcher())
		}
	})
	return p.previews
}

// NewProgram builds the bridge. The transport is optional: a Program with no
// transport is a local-only UI, which is what the renderer tests want.
func NewProgram(window string, tr *Transport) *Program {
	p := &Program{model: New(window), tr: tr}
	if tr != nil {
		tr.SetActive(window)
	}
	return p
}

// WithSettings gives the program a settings file. It is a method rather than a fourth
// argument to NewProgram because every existing caller wants the defaults and does not care
// where they are stored.
func (p *Program) WithSettings(path string) *Program {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.model = p.model.WithSettings(path)
	p.rev++
	return p
}

// Model returns the current model, for a driver that needs to inspect it.
//
// The returned value shares its maps with the live model, so it is a snapshot only while
// the lock is held: callers must not keep it and read it later, which is what a race
// detector will happily point out. It exists for tests and for drivers that render
// immediately; the front ends render through View().
func (p *Program) Model() Model {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.model
}

// Step applies one message and returns the command the model asked for. A
// submitted line is handed to the transport here, which is the only place the
// two halves meet.
//
// resolve is true: a caller with no bubbletea runtime (a test, a headless driver) gets the
// model's command run and its Sent message applied immediately, or pressing Enter would
// put nothing on screen. Under a real runtime Update is used instead, and bubbletea runs
// the command itself.
func (p *Program) Step(msg tea.Msg) tea.Cmd {
	return p.step(msg, true, true)
}

// Update is bubbletea's entry point. It must NOT resolve the model's command: the runtime
// already does that, feeds the resulting Sent back in here, and resolving it a second time
// applies the local echo twice. That was the duplicated message - the same line appended
// once by step() and once by the runtime's own dispatch of the command it returned.
func (p *Program) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	return p, p.step(msg, false, true)
}

// step is Step with the command resolution and the active-window sync made optional.
func (p *Program) step(msg tea.Msg, resolve, sync bool) tea.Cmd {
	// A Sent has crossed the model already: the local echo was applied when it was produced,
	// and all that is left is the wire. It is handled here, before the model sees it, so the
	// line is sent exactly once whether it arrives from a runtime or from step() itself.
	if sent, ok := msg.(Sent); ok {
		p.submit(sent.Text)
	}

	// Settings are the shared truth at the bouncer. The model produces the token
	// because only it knows the current settings; the driver owns the socket, so
	// the line is written here and nowhere else.
	if push, ok := msg.(PrefsPush); ok {
		p.sendPrefs(push.Token)
	}

	// A request for older history is resolved here for the same reason a Sent is: the model has
	// no socket. The transport knows how to say "older than this" on this wire, and CHATHISTORY
	// is the only form of it the protocol offers - so the model asks for history and the
	// transport decides how to ask.
	if want, ok := msg.(HistoryWanted); ok {
		p.requestHistory(want)
	}

	p.mu.Lock()
	next, cmd := p.model.Step(msg)
	p.model = next
	p.rev++
	p.mu.Unlock()

	if cmd != nil && resolve {
		// The model answers Enter with a command that resolves to Sent. With no runtime to
		// resolve it, do it here and apply the Sent to the model, or the local echo never
		// happens: the user presses Enter, the line goes to the bouncer, and their screen
		// stays empty until the bouncer echoes it - which it only does with echo-message,
		// which salotto deliberately does not request upstream.
		//
		// A /fset command resolves to PrefsPush instead. Re-entering through step gives both
		// the one place that knows how to put each on the wire, and the model's own state
		// update has already happened.
		switch out := cmd().(type) {
		case Sent:
			// resolve=false because this Sent is already the resolution.
			p.step(out, false, false)
		case PrefsPush:
			p.step(out, false, false)
		}
	}

	if sync {
		// Keep the transport pointed at whatever window the user is looking at.
		// Doing this here rather than asking drivers to call SyncActive is not
		// tidiness: a driver that forgets sends the next message to the window
		// the user left, which is the quietest possible bug in a chat client.
		p.SyncActive()
	}

	// Links in the window the user is looking at get a preview fetched in the
	// background. It is fired from every Step rather than from the frame builder
	// because the frame builder must never wait on a stranger's web server.
	p.primePreviews()

	return cmd
}

// submit turns typed text into a wire line and queues it.
func (p *Program) submit(text string) {
	if p.tr == nil {
		return
	}
	line := p.tr.Command(text)
	if line == "" {
		// Nothing to send: a bare line with no window to say it in. The local echo has
		// already been drawn, so the user sees what they typed and it simply does not go
		// anywhere - which is better than a mangled command reaching the server.
		return
	}
	p.tr.Send(line)
}

// sendPrefs writes a settings blob to the bouncer. A token with no destination
// is impossible: the transport is the only destination, and a local-only
// Program has no transport, so the file remains the whole story there.
func (p *Program) sendPrefs(token string) {
	if p.tr == nil || token == "" {
		return
	}
	p.tr.Send("PREF SET " + token)
}

// requestHistory asks the bouncer for lines older than the cursor.
//
// CHATHISTORY BEFORE takes an ISO8601 timestamp, and the cursor is already one: the client's own
// oldest line in that window, which came from a server-time tag and is therefore the same clock
// the bouncer stamped the logs with. That is what makes paging work - the cursor is not "about
// when I arrived", it is a timestamp the server itself produced.
//
// The limit is a page, not everything: a client that asks for ten years of history in one reply
// makes the bouncer read every log file before it can answer. Paging is the same request repeated
// as the user scrolls, which is how much history they actually wanted.
func (p *Program) requestHistory(want HistoryWanted) {
	if p.tr == nil || want.Window == "" || want.Before.IsZero() {
		return
	}
	// UTC with the exact nanosecond form the bouncer parses. The wire form is the protocol's,
	// not Go's default: a space instead of "T" would be a timestamp the server rejects, and a
	// rejected CHATHISTORY is silence rather than an error the user can see.
	cursor := want.Before.UTC().Format("2006-01-02T15:04:05.000000Z")
	// The bare window name, not the model's internal key: CHATHISTORY takes a window name and
	// resolves the network itself, and "libera/#c" is not a window any bouncer has. Sending
	// the name also means a request for one network's #c can be answered by whichever network
	// holds it, which is the same "a read is free to look" rule the bouncer already follows.
	p.tr.Send("CHATHISTORY BEFORE " + winName(want.Window) + " timestamp=" + cursor + " 100")
}

// WindowName is the active window, so a driver can tell where the user is.
//
// It returns the model's key, which is "network/name" once the window's server is known. That
// is deliberate: a driver that wants to switch back has to be able to name the window
// unambiguously, and two networks' "status" cannot be told apart by name. Anything that puts a
// window in front of a user goes through winName first.
func (p *Program) WindowName() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.model.Active()
}

// SetActiveWindow switches windows by name, which is what a web request knows:
// the browser sends a window, not a keystroke. It goes through the model so the
// scroll position and input buffer behave the same as a Ctrl-N in the TUI.
func (p *Program) SetActiveWindow(name string) {
	p.mu.Lock()
	p.model = p.model.SetActive(name)
	// Every state change bumps the revision, or an attached browser never learns
	// about it: the poller in ServeWS watches this counter and nothing else.
	p.rev++
	p.mu.Unlock()
	// The transport must follow, or the browser's next line goes to whichever
	// window the last keystroke left active.
	p.SyncActive()
}

// SyncActive keeps the transport pointed at the window the user switched to, so a
// bare line goes to the right place.
func (p *Program) SyncActive() {
	if p.tr == nil {
		return
	}
	// The transport sees the bare window name. It speaks IRC, which has no idea what a
	// "network/name" key is - the bouncer resolves a channel across its networks itself.
	p.tr.SetActive(winName(p.WindowName()))
}

// View renders the current model.
func (p *Program) View() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.model.View()
}

// Revision reports how many messages have been applied, which is how a poller
// notices that the render changed. It is deliberately coarse: a UI needs "it
// changed", not a change log.
func (p *Program) Revision() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rev
}

// SetInput replaces the input buffer, for a client mirroring what is being typed.
// It is not a submit: the line is still uncommitted.
func (p *Program) SetInput(text string) {
	p.mu.Lock()
	p.model = p.model.SetInput(text)
	p.rev++
	p.mu.Unlock()
}

// Search runs a scrollback search. The results are the model's own type: there is no
// wire form any more, because there is no JSON frame that has to describe them.
func (p *Program) Search(query string) []SearchResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.model.Search(query)
}

// primePreviews fetches a card for each link in the window the user is looking at, in
// the background, and delivers it to the model as a message.
//
// Server-side and once per link, for the reason in preview.go: fetching in the browser
// tells every link's author who read the channel and from which address. It is fired
// from Step so the model never waits on a stranger's web server, and the card arrives as
// PreviewReady so it lands in the same scrollback as the message that carried the link -
// which is what makes it show up in the SSH and terminal front ends too.
func (p *Program) primePreviews() {
	cache := p.previewCache()
	p.mu.Lock()
	window := p.model.Active()
	links := p.model.Links(window, previewsPerWindow)
	p.mu.Unlock()
	for _, u := range links {
		if _, ok := cache.Cached(u); ok {
			continue
		}
		go func(link string) {
			ctx, cancel := context.WithTimeout(context.Background(), previewTimeout)
			defer cancel()
			// Get is silent on failure: a dead link returns a Preview with no title.
			pv := cache.Get(ctx, link)
			if pv.Title == "" && pv.Site == "" {
				return
			}
			// Through Step, so the card is applied by the model under the same lock as
			// everything else: appending from this goroutine would be a second writer.
			p.Step(PreviewReady{Window: window, URL: link, Title: pv.Title, Site: pv.Site})
		}(u)
	}
}

// Next waits for the next message from the transport, blocking. A driver runs it
// in a loop; the web handler runs it with a timeout.
func (p *Program) Next() (tea.Msg, bool) {
	if p.tr == nil {
		return nil, false
	}
	msg, ok := <-p.tr.Msgs()
	return msg, ok
}

// Init satisfies tea.Model. There is no start-up command: the transport is
// already reading by the time a front end runs, because a bouncer connection
// owned by the model would be a connection the model could not close.
func (p *Program) Init() tea.Cmd { return nil }
