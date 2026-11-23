package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

// previewsInFrame is how many link cards one frame carries. Three is a screenful of
// conversation; more would make a frame bigger than the render it accompanies.
const previewsInFrame = 3

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

	// Body is the conversation as lines, already ANSI, without a frame. The browser
	// draws its own chrome, so it gets the lines and not the TUI's box around them -
	// which the page used to wrap in a second box.
	Body []string `json:"body,omitempty"`

	// Cols and Rows are the size this frame was rendered at, so the client knows
	// whether it needs to ask for a re-render when its viewport changes.
	Cols int `json:"cols,omitempty"`
	Rows int `json:"rows,omitempty"`

	// Key is a keystroke's name ("enter", "up", "backspace", ...) and Runes the
	// printable text of one. They travel instead of the whole buffer because the
	// model owns the buffer: a client that sent text would be a second one.
	Key   string `json:"key,omitempty"`
	Runes string `json:"runes,omitempty"`

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

	// Previews are the link cards for the active window, from the cache only. A
	// frame must never wait on a third-party server: a slow link would otherwise be
	// indistinguishable from a hung bouncer.
	Previews []Preview `json:"previews,omitempty"`

	// Topic is the active window's topic. State, not a line: it is replaced rather
	// than appended, and the client shows one.
	Topic string `json:"topic,omitempty"`

	// Modes is the active window's channel mode string ("+nt"). Sent beside the
	// topic because together they answer "what is this channel", and a user who
	// cannot see +m cannot tell why their message did not appear.
	Modes string `json:"modes,omitempty"`

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
		// The browser draws its own chrome, so it gets the conversation without the
		// TUI's frame. Rendering View() into a page that has a sidebar, a header, an
		// input form and a footer put a box inside a box and printed the input line
		// twice.
		cols, rows := p.model.Width(), p.model.Height()
		if cols < 20 {
			cols = 20
		}
		bodyRows := rows - 3 // the page's header and input, not the render's own
		if bodyRows < 3 {
			bodyRows = 3
		}
		return Frame{
			Type:       "state",
			Body:       p.model.Body(cols, bodyRows),
			Cols:       cols,
			Rows:       rows,
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
			Topic:      p.model.Topic(p.model.Active()),
			Previews:   p.cachedPreviews(),
			Modes:      p.model.Modes(p.model.Active()),
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
		case "resize":
			// The browser is the only thing that knows its viewport, and a render
			// wider than the screen wraps. Asking on every change is cheap: the
			// model only stores two ints.
			if f.Cols > 0 && f.Rows > 0 {
				p.Resize(f.Cols, f.Rows)
			}
		case "key":
			// A keystroke, not text: the browser sends what was pressed and the model
			// decides what it means. This is the same door the TUI uses, so the
			// buffer, history, completion and /command handling cannot diverge
			// between the two front ends.
			p.Key(f.Key, f.Runes)
		case "input":
			// Kept for a client that sets the whole buffer at once. Typing goes
			// through "key" now.
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

// cachedPreviews returns the previews already known for the active window.
//
// It never fetches. Fetching happens on its own goroutine when a line arrives
// (see primePreviews), because a handler that blocks on somebody else's web server
// is a handler that can be made to hang by a stranger.
// It is called from state(), which holds p.mu, so it must not take it again.
func (p *Program) cachedPreviews() []Preview {
	links := p.model.Links(p.model.Active(), previewsInFrame)
	if len(links) == 0 {
		return nil
	}
	cache := p.previews
	if cache == nil {
		return nil
	}
	out := make([]Preview, 0, len(links))
	for _, u := range links {
		if pp, ok := cache.Cached(u); ok && pp.Kind == "link" {
			out = append(out, pp)
		}
	}
	return out
}

// primePreviews fetches previews for links in the active window that are not known
// yet, in the background. It is called after each Step, and it is deliberately
// fire-and-forget: a preview is decoration, and the message must appear whether or
// not the third party answers.
func (p *Program) primePreviews() {
	cache := p.previewCache()
	p.mu.Lock()
	links := p.model.Links(p.model.Active(), previewsInFrame)
	p.mu.Unlock()
	for _, u := range links {
		if _, ok := cache.Cached(u); ok {
			continue
		}
		go func(link string) {
			ctx, cancel := context.WithTimeout(context.Background(), previewTimeout)
			defer cancel()
			cache.Get(ctx, link)
			// Bump the revision so a frame carries the new card. Without this the
			// preview would be cached and never shown, which looks exactly like a
			// feature that does not work.
			p.mu.Lock()
			p.rev++
			p.mu.Unlock()
		}(u)
	}
}

// stateJSON keeps the frame encoding in one place so the JS and the Go cannot
// disagree about field names.
func (f Frame) stateJSON() ([]byte, error) { return json.Marshal(f) }
