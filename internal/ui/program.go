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
		if sent, ok := cmd().(Sent); ok {
			p.mu.Lock()
			p.model, _ = p.model.Step(sent)
			p.mu.Unlock()
			p.submit(sent.Text)
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

// WindowName is the active window, so a driver can tell where the user is.
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
	p.tr.SetActive(p.WindowName())
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
