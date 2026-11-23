package ui

import (
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
func (p *Program) Model() Model {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.model
}

// Step applies one message and returns the command the model asked for. A
// submitted line is handed to the transport here, which is the only place the
// two halves meet.
func (p *Program) Step(msg tea.Msg) tea.Cmd {
	return p.step(msg, true)
}

// step is Step with the active-window sync made optional, so the internal
// re-feed of a Sent message does not do the same work twice.
func (p *Program) step(msg tea.Msg, sync bool) tea.Cmd {
	p.mu.Lock()
	next, cmd := p.model.Step(msg)
	p.model = next
	p.mu.Unlock()

	if cmd != nil {
		// The model answers Enter with a command that resolves to Sent. In a real
		// Program the runtime resolves it and feeds it back through Update; here
		// it is resolved immediately and fed back to the model, because otherwise
		// the local echo never happens: the user would press Enter, the line would
		// go to the bouncer, and their own screen would stay empty until the
		// bouncer echoed it — which it only does with echo-message, which salotto
		// deliberately does not request upstream.
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
	return cmd
}

// submit turns typed text into a wire line and queues it.
func (p *Program) submit(text string) {
	if p.tr == nil {
		return
	}
	line := p.tr.Command(text)
	if line != "" {
		p.tr.Send(line)
	}
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

// Update satisfies tea.Model, so a front end can run this as a bubbletea Program
// directly. It returns the same *Program rather than a copy, so the runtime's
// assertion back to the value it started with always succeeds.
func (p *Program) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	return p, p.Step(msg)
}
