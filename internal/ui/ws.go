package ui

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

// Frame is one message on the WebSocket. SPEC.md §13 calls for the same frames as
// the IRC wire, just carried over a socket: the browser gets rendered text and the
// window list, not a second protocol with its own semantics.
//
// The split is deliberate and is the whole reason this file is small. The server
// renders with the SAME View() the TUI and the SSH session use, so the browser
// cannot drift from the terminal. What it cannot get from a rendered string is
// structure - which windows exist, which is active, what the user typed - so those
// travel as fields alongside it.
type Frame struct {
	Type string `json:"type"`

	// Render is the terminal output, already ANSI, for the client to convert.
	// Sending the model's own View() verbatim is what keeps one renderer.
	Render string `json:"render,omitempty"`

	// Windows is the window list in cycle order, Active is the focused one.
	Windows []string `json:"windows,omitempty"`
	Active  string   `json:"active,omitempty"`

	// Unread is the per-window count of lines that arrived while the user was not
	// looking, and Mentions marks the windows where they were addressed. Two
	// fields because they answer two questions, and the sidebar draws them
	// differently: a count is information, a mention is a summons.
	Unread   map[string]int  `json:"unread,omitempty"`
	Mentions map[string]bool `json:"mentions,omitempty"`

	// SearchResults is populated only in reply to a "search" request.
	SearchResults []SearchHit `json:"results,omitempty"`
	Query         string      `json:"query,omitempty"`

	// Nicks is the membership of the active window and NickCounts the size of every
	// channel, so a sidebar can show how busy a channel is without being sent every
	// name in it. Only the active window's full list travels: a browser showing one
	// channel needs one nicklist.
	Nicks      []Nick         `json:"nicks,omitempty"`
	NickCounts map[string]int `json:"nickcounts,omitempty"`

	// Input mirrors the model's input buffer, so a second device attached to the
	// same bouncer sees what is being typed. Empty means nothing in flight.
	Input string `json:"input,omitempty"`

	// Text is what the client sends: a line to submit, or a command.
	Text string `json:"text,omitempty"`

	// Connected is the transport's report, never a guess: a front end that shows
	// "connected" while the bouncer is gone is the failure this whole UI work
	// exists to avoid.
	Connected bool   `json:"connected,omitempty"`
	Status    string `json:"status,omitempty"`
}

// SearchHit is one search result on the wire: which window, who, and what.
type SearchHit struct {
	Window string `json:"window"`
	Nick   string `json:"nick"`
	Text   string `json:"text"`
}

// upgrader is permissive about the origin because the front end is served from the
// same binary on the same host: there is no third-party page to defend against,
// and a strict check would break the deployment behind a proxy that rewrites Host.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// ServeWS upgrades a request and drives the Program over the socket.
//
// It is a driver exactly like the SSH session and the web form: it feeds
// tea.Msg into Program.Step and pushes Program.View() back out. Nothing here
// knows what a channel or a nick is, which is why adding real-time updates did not
// require touching the model.
func (p *Program) ServeWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote a response.
		return
	}
	defer conn.Close()

	// One writer goroutine per connection: gorilla permits exactly one concurrent
	// writer, and the reader would otherwise race it.
	var writeMu sync.Mutex
	write := func(f Frame) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return conn.WriteJSON(f)
	}

	// state renders the model and reports the revision it rendered. Both come out
	// of one lock acquisition so the pair is consistent: a revision read after the
	// render could miss a change that landed in between, and the client would then
	// wait for an update that had already happened.
	state := func() (Frame, uint64) {
		p.mu.Lock()
		defer p.mu.Unlock()
		return Frame{
			Type:       "state",
			Render:     p.model.View(),
			Windows:    p.model.Windows(),
			Active:     p.model.Active(),
			Input:      p.model.Input(),
			Connected:  p.model.Connected(),
			Status:     p.model.Status(),
			Unread:     p.model.Unread(),
			Mentions:   p.model.Mentions(),
			Nicks:      p.model.Nicklist(p.model.Active()),
			NickCounts: p.model.NickCounts(),
		}, p.rev
	}

	// Attach: the first frame is always the current state, so a reconnecting
	// browser redraws without asking. The revision it was rendered at is the
	// poller's baseline: taking the baseline any later would swallow a command the
	// client sent immediately after connecting, which is exactly what a browser
	// does when it restores a half-typed line or switches window on load.
	first, baseline := state()
	if err := write(first); err != nil {
		return
	}

	done := make(chan struct{})
	defer close(done)

	// push sends a fresh state whenever the model changes. The Program bumps a
	// revision on every Step, and this polls it: a condition variable across two
	// goroutines that both hold the model lock is more machinery than a bouncer UI
	// needs, and dropping a frame is harmless because the next one is a full
	// render.
	go func() {
		tick := time.NewTicker(120 * time.Millisecond)
		defer tick.Stop()
		last := baseline
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				if p.Revision() == last {
					continue
				}
				f, rev := state()
				last = rev
				if err := write(f); err != nil {
					return
				}
			}
		}
	}()

	// Read side: the client sends lines and window switches.
	for {
		var f Frame
		if err := conn.ReadJSON(&f); err != nil {
			return
		}
		switch f.Type {
		case "send":
			// The same path a keystroke takes, so command mapping, local echo and
			// window routing happen in exactly one place.
			for _, ru := range f.Text {
				p.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ru}})
			}
			p.Step(tea.KeyMsg{Type: tea.KeyEnter})
		case "select":
			if f.Active != "" {
				p.SetActiveWindow(f.Active)
			}
		case "input":
			// Typing is mirrored but not committed: replace the buffer.
			p.SetInput(f.Text)
		case "search":
			// Search is answered in a frame of its own rather than through the
			// model's revision counter: it is a question the client asked, not a
			// change in state, and a render-tick would make the answer arrive some
			// indeterminate time later.
			hits := p.Search(f.Query)
			out := Frame{Type: "results", Query: f.Query, SearchResults: hits}
			if err := write(out); err != nil {
				return
			}
		}
		// Every command produces a state frame on the next tick, so no reply is
		// needed here; the client is already subscribed to the render.
	}
}

// stateJSON keeps the frame encoding in one place so the JS and the Go cannot
// disagree about field names.
func (f Frame) stateJSON() ([]byte, error) { return json.Marshal(f) }
