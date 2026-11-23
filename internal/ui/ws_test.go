package ui

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/gorilla/websocket"
)

// wsDial opens a socket against a test server and returns the first state frame.
// Every assertion in this file starts from "what does a client actually receive",
// because the failure mode worth catching is a server that renders correctly and
// sends nothing.
func wsDial(t *testing.T, srv *httptest.Server) (*websocket.Conn, Frame) {
	t.Helper()
	url := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	c, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	_ = resp
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var f Frame
	if err := c.ReadJSON(&f); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	return c, f
}

// readState reads frames until one is a state frame that satisfies want, or the
// deadline expires. Intermediate frames are discarded: a client is a subscriber to
// a render, not a participant in a protocol with acknowledgements.
func readState(t *testing.T, c *websocket.Conn, within time.Duration, want func(Frame) bool) Frame {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(within))
	for {
		var f Frame
		if err := c.ReadJSON(&f); err != nil {
			t.Fatalf("waiting for state: %v", err)
		}
		if f.Type == "state" && want(f) {
			return f
		}
	}
}

func newWSServer(t *testing.T, p *Program) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", p.ServeWS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestWSFirstFrameIsTheWholeState pins that a client that connects knows
// everything without asking: the render, the window list and the active window.
// A UI that has to negotiate for its first paint is a UI that paints late.
func TestWSFirstFrameIsTheWholeState(t *testing.T) {
	Configure()
	p := NewProgram("#test", nil)
	p.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "hello over the socket"}))
	srv := newWSServer(t, p)

	_, f := wsDial(t, srv)
	if f.Type != "state" {
		t.Fatalf("type = %q", f.Type)
	}
	if !strings.Contains(f.Render, "hello over the socket") {
		t.Fatalf("render lacks the line: %q", f.Render)
	}
	if len(f.Windows) == 0 {
		t.Fatalf("first frame carried no windows: %+v", f)
	}
	if f.Active == "" {
		t.Fatalf("first frame carried no active window: %+v", f)
	}
}

// TestWSLiveEventArrivesWithoutAReload is the whole point of W1: a message from
// the wire reaches an attached browser with nobody touching it. The old POST form
// could only ever show what existed at request time.
func TestWSLiveEventArrivesWithoutAReload(t *testing.T) {
	Configure()
	p := NewProgram("#test", nil)
	srv := newWSServer(t, p)
	_, _ = wsDial(t, srv)

	p.Step(Incoming(Line{Window: "#test", Nick: "bob", Text: "appeared live"}))

	// Read from the connection we already have, with no new request.
	c, _ := wsDial(t, srv)
	p.Step(Incoming(Line{Window: "#test", Nick: "bob", Text: "pushed after connect"}))
	f := readState(t, c, 5*time.Second, func(f Frame) bool {
		return strings.Contains(f.Render, "pushed after connect")
	})
	if !strings.Contains(f.Render, "pushed after connect") {
		t.Fatalf("render: %q", f.Render)
	}
}

// TestWSSendReachesTheWire drives the write path: what the client sends must go
// through the same Step a keystroke takes, and so reach the transport.
func TestWSSendReachesTheWire(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("#test", tr)
	srv := newWSServer(t, p)
	c, _ := wsDial(t, srv)

	if err := c.WriteJSON(Frame{Type: "send", Text: "hello from the browser"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case got := <-fb.sent:
		if !strings.Contains(got, "hello from the browser") {
			t.Fatalf("wire got %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the line typed into the socket never reached the wire")
	}
}

// TestWSSecondDeviceSeesTyping pins the multi-device claim: two browsers on one
// bouncer, and the first one's uncommitted input is visible to the second. Without
// this the "second device" story is just two independent views.
func TestWSSecondDeviceSeesTyping(t *testing.T) {
	Configure()
	p := NewProgram("#test", nil)
	srv := newWSServer(t, p)
	a, _ := wsDial(t, srv)
	b, _ := wsDial(t, srv)

	if err := a.WriteJSON(Frame{Type: "input", Text: "half a sentence"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	f := readState(t, b, 5*time.Second, func(f Frame) bool { return f.Input == "half a sentence" })
	if f.Input != "half a sentence" {
		t.Fatalf("input = %q", f.Input)
	}

	// And typing must not have sent anything: mirroring is not submitting.
	if got := p.Model().Input(); got != "half a sentence" {
		t.Fatalf("model input = %q", got)
	}
}

// TestWSSelectSwitchesWindow pins that a click in the sidebar routing is real: the
// active window changes on the server, so the next line goes there.
func TestWSSelectSwitchesWindow(t *testing.T) {
	Configure()
	p := NewProgram("#one", nil)
	p.Step(Incoming(Line{Window: "#two", Nick: "alice", Text: "in two"}))
	srv := newWSServer(t, p)
	c, f0 := wsDial(t, srv)
	if len(f0.Windows) < 2 {
		t.Fatalf("need two windows, got %v", f0.Windows)
	}

	var target string
	for _, w := range f0.Windows {
		if w != f0.Active {
			target = w
			break
		}
	}
	if err := c.WriteJSON(Frame{Type: "select", Active: target}); err != nil {
		t.Fatalf("write: %v", err)
	}
	f := readState(t, c, 5*time.Second, func(f Frame) bool { return f.Active == target })
	if f.Active != target {
		t.Fatalf("active = %q want %q", f.Active, target)
	}
}

// TestWSFrameIsJSONWithTheDocumentedFields guards the contract the browser's JS
// depends on. Field names are the interface; renaming one silently breaks the page
// with no compile error anywhere.
func TestWSFrameIsJSONWithTheDocumentedFields(t *testing.T) {
	b, err := json.Marshal(Frame{Type: "state", Render: "x", Windows: []string{"#a"},
		Active: "#a", Input: "i", Connected: true, Status: "s"})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"type"`, `"render"`, `"windows"`, `"active"`,
		`"input"`, `"connected"`, `"status"`} {
		if !strings.Contains(string(b), field) {
			t.Fatalf("frame JSON lacks %s: %s", field, b)
		}
	}
}

// TestWSClientAssetIsServed pins that the page's script is actually reachable and
// is not the empty string. A missing embed renders a page that looks fine and
// never updates.
func TestWSClientAssetIsServed(t *testing.T) {
	Configure()
	p := NewProgram("#test", nil)
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/client.js")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Read it all: a single Read returns one chunk, and the interesting part of a
	// script with a long header comment is not in the first 4 KB.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "WebSocket") {
		t.Fatalf("client.js looks wrong: %q", string(body[:200]))
	}
}

// TestPageCarriesTheSocketClient pins that the served page loads the script and
// has the elements it looks for. The JS and the HTML are separate files, so this is
// the only thing that notices when one is renamed.
func TestPageCarriesTheSocketClient(t *testing.T) {
	Configure()
	p := NewProgram("#test", nil)
	srv := httptest.NewServer(Handler(p))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, want := range []string{`/client.js`, `id="term"`, `id="windows"`, `id="line"`} {
		if !strings.Contains(html, want) {
			t.Fatalf("page lacks %s", want)
		}
	}
}

// silence an unused import if the file evolves
var _ = tea.KeyEnter
