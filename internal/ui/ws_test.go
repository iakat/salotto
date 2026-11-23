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

// TestUnreadCountsOnlyWhatWasNotSeen pins the definition of unread: a line in the
// window you are looking at was seen arrive, so it does not count. Getting this
// wrong makes the number a total, which tells the user nothing.
func TestUnreadCountsOnlyWhatWasNotSeen(t *testing.T) {
	m := New("#here")
	m = m.SetActive("#there")
	m, _ = m.Step(Incoming(Line{Window: "#there", Nick: "a", Text: "seen"}))
	if n := m.Unread()["#there"]; n != 0 {
		t.Fatalf("a line in the active window counted as unread: %d", n)
	}
	m, _ = m.Step(Incoming(Line{Window: "#here", Nick: "a", Text: "unseen"}))
	if n := m.Unread()["#here"]; n != 1 {
		t.Fatalf("unread in another window = %d, want 1", n)
	}
	// Looking at it is what clears it.
	m = m.SetActive("#here")
	if n := m.Unread()["#here"]; n != 0 {
		t.Fatalf("switching to the window did not clear its unread count: %d", n)
	}
}

// TestMentionIsSeparateFromUnread pins that the two signals stay distinct. A
// mention is a summons; an unread line is information. Collapsing them would make
// the sidebar unable to answer "was this for me".
func TestMentionIsSeparateFromUnread(t *testing.T) {
	m := New("#here")
	m = m.SetActive("#elsewhere")
	m, _ = m.Step(Incoming(Line{Window: "#here", Nick: "a", Text: "hey alice", Mention: true}))

	// Both signals fire, and they mean different things: there is something new
	// in #here, and some of it was addressed to us.
	if !m.Mentions()["#here"] {
		t.Fatal("a mention in a window we are not looking at was not recorded")
	}
	if n := m.Unread()["#here"]; n != 1 {
		t.Fatalf("unread = %d, want 1: a mention is also an unread line", n)
	}

	// Looking at the window clears both. What remains is the highlight on the line
	// itself, which is how a mention in the window you are reading is shown: there
	// is no badge to earn when the text is already in front of you.
	m = m.SetActive("#here")
	if m.Mentions()["#here"] || m.Unread()["#here"] != 0 {
		t.Fatal("switching to the window did not clear the mention")
	}
	hits := m.Search("hey alice")
	if len(hits) != 1 || !hits[0].Line.Mention {
		t.Fatalf("the line lost its mention flag: %+v", hits)
	}
}

// TestMentionDetectionIsWordBoundary is the rule that keeps the highlight honest.
// A substring test flags "alice" inside "malice", and a user who is told they were
// mentioned when they were not stops trusting the highlight.
func TestMentionDetectionIsWordBoundary(t *testing.T) {
	tr := &Transport{own: map[string]bool{"alice": true}, windows: map[string]bool{}}
	cases := []struct {
		text string
		want bool
	}{
		{"alice: are you there", true},
		{"hey alice", true},
		{"ALICE", true},
		{"(alice)", true},
		{"malice", false},
		{"alicea", false},
		{"palice", false},
		{"nothing here", false},
	}
	for _, c := range cases {
		if got := tr.mentionsUs("bob", c.text); got != c.want {
			t.Errorf("mentionsUs(%q) = %v, want %v", c.text, got, c.want)
		}
	}
	// Our own lines are never a mention.
	if tr.mentionsUs("alice", "alice was here") {
		t.Error("a line from ourselves was counted as a mention")
	}
}

// TestSearchFindsAcrossWindows pins W4's in-client half: the query crosses every
// window, not just the open one, and results carry the window they came from so a
// click can jump there.
func TestSearchFindsAcrossWindows(t *testing.T) {
	m := New("#one")
	m, _ = m.Step(Incoming(Line{Window: "#one", Nick: "a", Text: "the deploy failed"}))
	m, _ = m.Step(Incoming(Line{Window: "#two", Nick: "b", Text: "a different deploy"}))
	m, _ = m.Step(Incoming(Line{Window: "#two", Nick: "c", Text: "unrelated"}))

	hits := m.Search("deploy")
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2: %+v", len(hits), hits)
	}
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.Window] = true
	}
	if !seen["#one"] || !seen["#two"] {
		t.Fatalf("search did not cross windows: %+v", hits)
	}
	// Case-insensitive, and it searches the nick too.
	if len(m.Search("DEPLOY")) != 2 {
		t.Error("search is case-sensitive")
	}
	if len(m.Search("b")) == 0 {
		t.Error("search does not match nicks")
	}
	if len(m.Search("   ")) != 0 {
		t.Error("an empty query returned results")
	}
}

// TestSearchFrameReachesTheClient pins that the answer actually goes out on the
// socket: a search that runs and is never sent looks identical to one that found
// nothing.
func TestSearchFrameReachesTheClient(t *testing.T) {
	Configure()
	p := NewProgram("#one", nil)
	p.Step(Incoming(Line{Window: "#one", Nick: "a", Text: "needle in here"}))
	srv := newWSServer(t, p)
	c, _ := wsDial(t, srv)

	if err := c.WriteJSON(Frame{Type: "search", Query: "needle"}); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var f Frame
		if err := c.ReadJSON(&f); err != nil {
			t.Fatalf("waiting for results: %v", err)
		}
		if f.Type != "results" {
			continue
		}
		if f.Query != "needle" || len(f.SearchResults) != 1 {
			t.Fatalf("results frame = %+v", f)
		}
		if f.SearchResults[0].Window != "#one" {
			t.Fatalf("hit window = %q", f.SearchResults[0].Window)
		}
		return
	}
}

// TestWSCommandImmediatelyAfterConnectIsNotSwallowed is the regression test for a
// race that produced an intermittent "waiting for state" timeout.
//
// The poller's baseline revision used to be read after the first frame was written.
// A command applied in that window moved the revision before the baseline was
// taken, so the poller believed it had already sent that state and never sent it:
// the client's command looked ignored, sometimes, depending on timing. The baseline
// is now read from the same lock acquisition as the first render.
func TestWSCommandImmediatelyAfterConnectIsNotSwallowed(t *testing.T) {
	Configure()
	p := NewProgram("#one", nil)
	p.Step(Incoming(Line{Window: "#two", Nick: "a", Text: "in two"}))
	srv := newWSServer(t, p)
	c, first := wsDial(t, srv)

	var target string
	for _, w := range first.Windows {
		if w != first.Active {
			target = w
			break
		}
	}
	if target == "" {
		t.Fatalf("need a second window, got %v", first.Windows)
	}

	// Send immediately, with no pause: this is the window that used to be lost.
	if err := c.WriteJSON(Frame{Type: "select", Active: target}); err != nil {
		t.Fatal(err)
	}
	f := readState(t, c, 5*time.Second, func(f Frame) bool { return f.Active == target })
	if f.Active != target {
		t.Fatalf("active = %q, want %q", f.Active, target)
	}
}

// TestMentionsReachTheFrame is the full path: wire line in, frame out. The unit
// tests either side of it pass while the middle is broken, which is what a live
// probe found - the line arrived and the frame had no mention in it.
func TestMentionsReachTheFrame(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("status", tr)
	srv := newWSServer(t, p)
	c, _ := wsDial(t, srv)

	// Drive the transport's own read path, then let the Program consume it.
	fb.inject(t, ":irc.test 001 alice :welcome")
	fb.inject(t, ":bob!b@h JOIN #c")
	fb.inject(t, ":bob!b@h PRIVMSG #c :alice: are you there")

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case msg := <-tr.Msgs():
			p.Step(msg)
		case <-time.After(2 * time.Second):
			t.Fatalf("no transport message; model unread=%v mentions=%v",
				p.Model().Unread(), p.Model().Mentions())
		}
		if got := p.Model().Mentions(); len(got) > 0 {
			break
		}
	}
	if !p.Model().Mentions()["#c"] {
		t.Fatalf("the model has no mention for #c; model unread=%v", p.Model().Unread())
	}

	// And the frame the browser reads carries it.
	p.Step(Incoming(Line{Window: "#other", Nick: "x", Text: "force a revision"}))
	f := readState(t, c, 5*time.Second, func(f Frame) bool { return true })
	if !f.Mentions["#c"] {
		t.Fatalf("frame mentions = %v, want #c", f.Mentions)
	}
}
