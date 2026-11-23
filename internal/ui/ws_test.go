package ui

import (
	"context"
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

// TestNamesReplyBecomesANicklist pins the parse of 353/366. The membership of a
// channel is not conversation: a 353 line in the scrollback would be noise, and the
// nickname list has to be a list, not lines to scrape.
func TestNamesReplyBecomesANicklist(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":irc.test 353 me = #c :@alice +bob carol")
	fb.inject(t, ":irc.test 366 me #c :End of /NAMES list")

	// No Nicklist until 366: a large channel is split across several 353 replies,
	// and publishing each one would redraw the list once per reply.
	got := collectNicks(t, tr)
	if got.Window != "#c" {
		t.Fatalf("nicklist for %q", got.Window)
	}
	if len(got.Nicks) != 3 {
		t.Fatalf("nicks = %+v", got.Nicks)
	}
	// Ops first, then alphabetical: a nicklist is read for "who is in charge".
	if got.Nicks[0].Name != "alice" || got.Nicks[0].Prefix != "@" {
		t.Fatalf("first nick = %+v, want @alice", got.Nicks[0])
	}
	if got.Nicks[1].Name != "bob" || got.Nicks[2].Name != "carol" {
		t.Fatalf("order = %+v", got.Nicks)
	}
}

// TestNicklistFollowsMembership pins that the list stays true after the initial
// NAMES: a nicklist that only ever reflects the join moment is worse than none,
// because it looks authoritative and is stale.
func TestNicklistFollowsMembership(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":irc.test 353 me = #c :alice bob")
	fb.inject(t, ":irc.test 366 me #c :End of /NAMES list")
	if n := collectNicks(t, tr); len(n.Nicks) != 2 {
		t.Fatalf("initial nicks = %+v", n.Nicks)
	}

	// Arrival.
	fb.inject(t, ":dave!d@h JOIN #c")
	if n := collectNicks(t, tr); len(n.Nicks) != 3 {
		t.Fatalf("after join nicks = %+v", n.Nicks)
	}

	// Departure.
	fb.inject(t, ":bob!b@h PART #c")
	if n := collectNicks(t, tr); len(n.Nicks) != 2 {
		t.Fatalf("after part nicks = %+v", n.Nicks)
	}

	// A rename moves the entry rather than duplicating it.
	fb.inject(t, ":dave!d@h NICK david")
	if n := collectNicks(t, tr); len(n.Nicks) != 2 {
		t.Fatalf("after nick nicks = %+v", n.Nicks)
	}

	// And a quit clears the nick from the list it was in.
	fb.inject(t, ":david!d@h QUIT :bye")
	if n := collectNicks(t, tr); len(n.Nicks) != 1 || n.Nicks[0].Name != "alice" {
		t.Fatalf("after quit nicks = %+v", n.Nicks)
	}
}

// collectNicks reads until a Nicklist arrives, failing if none does.
func collectNicks(t *testing.T, tr *Transport) Nicklist {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("transport closed before a nicklist arrived")
			}
			if n, ok := msg.(Nicklist); ok {
				return n
			}
		case <-deadline:
			t.Fatal("no nicklist arrived")
		}
	}
}

// TestNicklistReachesTheFrame pins that the browser is told. The sidebar draws its
// member list from the frame, so a nicklist the frame lacks is a nicklist the user
// does not have.
func TestNicklistReachesTheFrame(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "hi"}))
	p.Step(Nicklist{Window: "#c", Nicks: []Nick{
		{Name: "alice", Prefix: "@"}, {Name: "bob"},
	}})

	srv := newWSServer(t, p)
	_, f := wsDial(t, srv)
	if len(f.Nicks) != 2 {
		t.Fatalf("frame nicks = %+v", f.Nicks)
	}
	if f.Nicks[0].Name != "alice" || f.Nicks[0].Prefix != "@" {
		t.Fatalf("frame nicks = %+v", f.Nicks)
	}
	if f.NickCounts["#c"] != 2 {
		t.Fatalf("nickcounts = %v", f.NickCounts)
	}
}

// TestNicklistReplacesRatherThanMerges pins the update semantics. The transport
// sends the complete list, so merging would keep people who left - the failure mode
// of every nicklist that is "maintained" instead of "replaced".
func TestNicklistReplacesRatherThanMerges(t *testing.T) {
	m := New("#c")
	m, _ = m.Step(Nicklist{Window: "#c", Nicks: []Nick{{Name: "alice"}, {Name: "bob"}}})
	if got := m.Nicklist("#c"); len(got) != 2 {
		t.Fatalf("nicks = %+v", got)
	}
	m, _ = m.Step(Nicklist{Window: "#c", Nicks: []Nick{{Name: "alice"}}})
	if got := m.Nicklist("#c"); len(got) != 1 || got[0].Name != "alice" {
		t.Fatalf("after replace nicks = %+v", got)
	}
	// An empty list means the channel has nobody: the entry goes away.
	m, _ = m.Step(Nicklist{Window: "#c"})
	if got := m.Nicklist("#c"); len(got) != 0 {
		t.Fatalf("after empty nicks = %+v", got)
	}
}

// TestNickJSONFieldNames is the regression test for a bug every Go-side test
// missed: Nick had no JSON tags, so it marshalled as {"Name":..,"Prefix":..} while
// the browser client reads "name"/"prefix". The nicklist rendered as "undefined"
// in a real browser and was invisible to any test that only inspected Go values.
//
// The field names are the interface. This pins them on the wire, not in the struct.
func TestNickJSONFieldNames(t *testing.T) {
	b, err := json.Marshal(Nick{Name: "alice", Prefix: "@"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"name":"alice","prefix":"@"}` {
		t.Fatalf("Nick marshals to %s; the browser reads lowercase field names", got)
	}
	// An ordinary member carries no prefix key at all, so the client's
	// `n.prefix || ""` is the only thing needed there.
	b, err = json.Marshal(Nick{Name: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != `{"name":"bob"}` {
		t.Fatalf("a plain Nick marshals to %s", got)
	}
}

// TestTopicReplyBecomesState pins that a topic is state, not scrollback. A 332 in
// the conversation would leave every old topic on screen forever, and the current
// one would be indistinguishable from a description of the past.
func TestTopicReplyBecomesState(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":irc.test 332 me #c :the current topic")

	got := collectTopic(t, tr)
	if got.Window != "#c" || got.Text != "the current topic" {
		t.Fatalf("topic = %+v", got)
	}
}

// TestNoTopicIsARealState pins that 331 is recorded rather than ignored. "This
// channel has no topic" is an answer; treating it as silence means a channel whose
// topic was just cleared keeps showing the old one.
func TestNoTopicIsARealState(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":irc.test 331 me #c :No topic is set")

	got := collectTopic(t, tr)
	if got.Window != "#c" || got.Text != "" {
		t.Fatalf("topic = %+v", got)
	}
}

// TestTopicChangeUpdatesStateAndSaysSo pins both halves of a live topic change:
// the header is replaced AND the window shows what happened. "The topic changed" is
// precisely the moment a user wants to know what it changed to.
func TestTopicChangeUpdatesStateAndSaysSo(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":alice!a@h TOPIC #c :a new topic")

	// The state message and the line both arrive; order is not asserted because
	// they are two independent messages about one event.
	deadline := time.After(5 * time.Second)
	var sawTopic, sawLine bool
	for !sawTopic || !sawLine {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("transport closed")
			}
			switch m := msg.(type) {
			case Topic:
				if m.Window == "#c" && m.Text == "a new topic" {
					sawTopic = true
				}
			case Incoming:
				if Line(m).Window == "#c" && strings.Contains(Line(m).Text, "a new topic") {
					sawLine = true
				}
			}
		case <-deadline:
			t.Fatalf("sawTopic=%v sawLine=%v", sawTopic, sawLine)
		}
	}
}

// collectTopic reads until a Topic message arrives.
func collectTopic(t *testing.T, tr *Transport) Topic {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("transport closed before a topic arrived")
			}
			if tp, ok := msg.(Topic); ok {
				return tp
			}
		case <-deadline:
			t.Fatal("no topic message arrived")
		}
	}
}

// TestTopicReachesTheFrame pins that the browser is told, and that it is a field
// rather than a line: the client draws one topic, so a missing field means no
// header at all.
func TestTopicReachesTheFrame(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "hi"}))
	p.Step(Topic{Window: "#c", Text: "welcome to the channel"})

	srv := newWSServer(t, p)
	_, f := wsDial(t, srv)
	if f.Topic != "welcome to the channel" {
		t.Fatalf("frame topic = %q", f.Topic)
	}
}

// TestTopicSurvivesAWindowSwitch pins the header follows the window. A topic shown
// for the wrong channel is worse than none: it is confidently wrong.
func TestTopicSurvivesAWindowSwitch(t *testing.T) {
	Configure()
	p := NewProgram("#one", nil)
	p.Step(Incoming(Line{Window: "#two", Nick: "a", Text: "hi"}))
	p.Step(Topic{Window: "#one", Text: "topic one"})
	p.Step(Topic{Window: "#two", Text: "topic two"})

	srv := newWSServer(t, p)
	c, f := wsDial(t, srv)
	if f.Topic != "topic one" {
		t.Fatalf("on #one the topic is %q", f.Topic)
	}

	if err := c.WriteJSON(Frame{Type: "select", Active: "#two"}); err != nil {
		t.Fatal(err)
	}
	f = readState(t, c, 5*time.Second, func(f Frame) bool { return f.Active == "#two" })
	if f.Topic != "topic two" {
		t.Fatalf("on #two the topic is %q", f.Topic)
	}
}

// TestChannelModesAreState pins that 324 is recorded and republished, not written
// into the conversation: "+nt" is the current answer, and a scrollback full of every
// mode string the channel ever had tells a user nothing about now.
func TestChannelModesAreState(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":irc.test 324 me #c +nt")

	got := collectModes(t, tr)
	if got.Window != "#c" || got.Modes != "+nt" {
		t.Fatalf("modes = %+v", got)
	}
}

// TestModeGrantsAPrefix pins the operation a user actually notices: somebody gets
// opped and the nicklist shows it. Without this the @ appears only after a rejoin.
func TestModeGrantsAPrefix(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":irc.test 353 me = #c :alice bob")
	fb.inject(t, ":irc.test 366 me #c :End of /NAMES list")
	collectNicklist(t, tr)

	fb.inject(t, ":chanop!o@h MODE #c +o bob")
	nl := collectNicklist(t, tr)

	for _, n := range nl.Nicks {
		if n.Name == "bob" && n.Prefix != "@" {
			t.Fatalf("bob has prefix %q, want @", n.Prefix)
		}
	}
}

// TestModeMultiChangeIsFullyApplied pins the multi-change line. "+o-v a b" is one
// line carrying two independent facts, and applying only the first is the kind of
// bug that shows up as "the ops list is wrong sometimes" and is never reproducible.
func TestModeMultiChangeIsFullyApplied(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":irc.test 353 me = #c :@alice +bob carol")
	fb.inject(t, ":irc.test 366 me #c :End of /NAMES list")
	collectNicklist(t, tr)

	fb.inject(t, ":chanop!o@h MODE #c +o-v alice bob")
	nl := collectNicklist(t, tr)

	byName := map[string]string{}
	for _, n := range nl.Nicks {
		byName[n.Name] = n.Prefix
	}
	if byName["alice"] != "@" {
		t.Fatalf("alice = %q, want @ (her prefix was already @)", byName["alice"])
	}
	if byName["bob"] != "" {
		t.Fatalf("bob = %q, want no prefix (devoiced)", byName["bob"])
	}
}

// TestChannelModesFoldAndSort pins that the mode string is a set, not a transcript.
// "+nt" and "+tn" are the same channel; a string that reorders on every update looks
// like it is changing when it is not.
func TestChannelModesFoldAndSort(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":irc.test 324 me #c +tn")
	if got := collectModes(t, tr); got.Modes != "+nt" {
		t.Fatalf("324 +tn folded to %q, want +nt", got.Modes)
	}

	fb.inject(t, ":op!o@h MODE #c +m")
	if got := collectModes(t, tr); got.Modes != "+mnt" {
		t.Fatalf("after +m the modes are %q, want +mnt", got.Modes)
	}
	fb.inject(t, ":op!o@h MODE #c -t")
	if got := collectModes(t, tr); got.Modes != "+mn" {
		t.Fatalf("after -t the modes are %q, want +mn", got.Modes)
	}

	// A mode with an argument it does not track (+b) must not eat the next mode
	// letter, and must not appear in the string: a ban is not a channel-wide flag
	// a header can show.
	// The mode string is contiguous ("+b-n mask"), not "+b mask -n": a server sends
	// all the changes of one MODE in one string, with the arguments after it.
	fb.inject(t, ":op!o@h MODE #c +b-n bad!*@*")
	if got := collectModes(t, tr); got.Modes != "+m" {
		t.Fatalf("after +b-n bad!*@* the modes are %q, want +m", got.Modes)
	}
}

// collectNicklist reads until a Nicklist message arrives.
func collectNicklist(t *testing.T, tr *Transport) Nicklist {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("transport closed before a nicklist arrived")
			}
			if nl, ok := msg.(Nicklist); ok {
				return nl
			}
		case <-deadline:
			t.Fatal("no nicklist arrived")
		}
	}
}

// collectModes reads until a ChannelModes message arrives.
func collectModes(t *testing.T, tr *Transport) ChannelModes {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("transport closed before a mode arrived")
			}
			if cm, ok := msg.(ChannelModes); ok {
				return cm
			}
		case <-deadline:
			t.Fatal("no channel modes arrived")
		}
	}
}

// TestModesReachTheFrame pins that the browser is told. A user who cannot see that a
// channel is +m cannot tell that their message did not appear for a reason.
func TestModesReachTheFrame(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "hi"}))
	p.Step(ChannelModes{Window: "#c", Modes: "+mnt"})

	srv := newWSServer(t, p)
	_, f := wsDial(t, srv)
	if f.Modes != "+mnt" {
		t.Fatalf("frame modes = %q", f.Modes)
	}
}

// TestPreviewsReachTheFrameOnlyFromCache pins the rule that a frame must never wait
// on a third-party server. A bouncer that blocks rendering on somebody else's web
// site can be made to hang by a stranger posting a slow link.
func TestPreviewsReachTheFrameOnlyFromCache(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "see https://example.invalid/one"}))

	// Nothing is cached yet; the frame must still be produced, with no previews.
	srv := newWSServer(t, p)
	_, f := wsDial(t, srv)
	if len(f.Previews) != 0 {
		t.Fatalf("frame carried %d previews before anything was fetched", len(f.Previews))
	}

	// With the card cached, it appears. The cache is injected so no test touches the
	// network.
	p.mu.Lock()
	p.previewsOnce.Do(func() {})
	p.previews = NewPreviewCache(staticFetcher{})
	p.previews.Get(context.Background(), "https://example.invalid/one")
	p.mu.Unlock()
	p.mu.Lock()
	p.rev++
	p.mu.Unlock()

	_, f = wsDial(t, srv)
	if len(f.Previews) != 1 || f.Previews[0].Title != "t" {
		t.Fatalf("frame previews = %+v", f.Previews)
	}
}

// TestPreviewOnlyForShownLines pins that previews come from recent lines in the
// active window, not from the whole scrollback. Fetching month-old links because a
// user opened a channel is the behaviour that makes previews a privacy problem.
func TestPreviewOnlyForShownLines(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "old https://example.invalid/old"}))
	for i := 0; i < 60; i++ {
		p.Step(Incoming(Line{Window: "#c", Nick: "b", Text: "filler"}))
	}
	p.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "new https://example.invalid/new"}))

	p.mu.Lock()
	links := p.model.Links(p.model.Active(), 10)
	p.mu.Unlock()

	if len(links) != 1 || !strings.Contains(links[0], "/new") {
		t.Fatalf("links = %v, want only the recent one", links)
	}
}

// TestLinksAreDeduped pins that one link mentioned five times is one fetch.
func TestLinksAreDeduped(t *testing.T) {
	m := New("#c")
	for i := 0; i < 5; i++ {
		m, _ = m.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "again https://example.invalid/x"}))
	}
	if got := m.Links("#c", 10); len(got) != 1 {
		t.Fatalf("Links = %v, want one entry", got)
	}
}

// TestPreviewsAreNotFetchedForOtherWindows pins that a link in a channel the user is
// not reading is not fetched. Previews follow attention; that is the whole point of
// doing them from rendered lines.
func TestPreviewsAreNotFetchedForOtherWindows(t *testing.T) {
	Configure()
	p := NewProgram("#here", nil)
	p.Step(Incoming(Line{Window: "#elsewhere", Nick: "a", Text: "https://example.invalid/away"}))
	p.mu.Lock()
	links := p.model.Links(p.model.Active(), 10)
	p.mu.Unlock()
	if len(links) != 0 {
		t.Fatalf("Links on the active window = %v, want none", links)
	}
}

// TestOwnJoinAsksForTheModes pins that we ask. 332 is volunteered by the server on
// JOIN; 324 is not, so a client that waits for it shows a topic and no modes - which
// looks like a channel with no modes rather than one nobody asked about.
func TestOwnJoinAsksForTheModes(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":me!m@h JOIN #c")

	// Read what the bouncer's side of the socket received: the claim is that we
	// ASKED, which only the wire can show.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case sent := <-fb.sent:
			if sent == "MODE #c" {
				return
			}
		case <-deadline:
			t.Fatal("the client never sent MODE #c after its own JOIN")
		}
	}
}

// TestFrameCarriesBodyNotJustRender pins the fix for the box inside a box. The page
// already has a sidebar, a header, an input row and a footer; handing it the TUI's
// framed View put a second frame around the conversation and a second input line
// under it.
func TestFrameCarriesBodyNotJustRender(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Resize(80, 24)
	p.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "hello there"}))

	srv := newWSServer(t, p)
	_, f := wsDial(t, srv)

	if len(f.Body) == 0 {
		t.Fatal("no body lines: the browser would have to draw the TUI's frame")
	}
	if f.Cols == 0 || f.Rows == 0 {
		t.Fatalf("frame reports %dx%d; the client cannot tell whether it needs a resize", f.Cols, f.Rows)
	}
	for _, line := range f.Body {
		// The TUI's frame characters must not be in the body: their presence is
		// exactly the box-in-a-box bug.
		if strings.ContainsAny(line, "\u256d\u256e\u2570\u256f") {
			t.Fatalf("body line carries the TUI border: %q", line)
		}
	}
	joined := strings.Join(f.Body, "\n")
	if !strings.Contains(joined, "hello there") {
		t.Fatalf("body does not contain the message: %q", joined)
	}
}

// TestResizeChangesWhatIsRendered pins that a resize actually reaches the model. A
// phone and a desktop get different line counts, and a client that reports its size
// and is ignored is worse than one that never reports it.
func TestResizeChangesWhatIsRendered(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Resize(40, 12)
	narrow := len(p.Model().Body(40, 12))

	p.Resize(120, 40)
	wide := len(p.Model().Body(120, 40))

	if wide <= narrow {
		t.Fatalf("a bigger viewport rendered %d lines, the smaller one %d", wide, narrow)
	}
	if p.Model().Width() != 120 || p.Model().Height() != 40 {
		t.Fatalf("model size is %dx%d", p.Model().Width(), p.Model().Height())
	}
}

// TestResizeHasAFloor pins that a hostile or silly size cannot make the render
// impossible: a zero-width window would divide by zero somewhere, and a negative one
// is a crash waiting for the first phone that reports it.
func TestResizeHasAFloor(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Resize(1, 1)
	if p.Model().Width() < 20 || p.Model().Height() < 6 {
		t.Fatalf("size is %dx%d, want at least 20x6", p.Model().Width(), p.Model().Height())
	}
	p.Resize(-5, -5)
	if p.Model().Width() < 20 || p.Model().Height() < 6 {
		t.Fatalf("negative size produced %dx%d", p.Model().Width(), p.Model().Height())
	}
}

// TestKeystrokesReachTheModelsBuffer pins that the browser types into the MODEL's
// buffer, not into a parallel string. That is what gives it the same history,
// completion and command handling as the TUI instead of a second implementation.
func TestKeystrokesReachTheModelsBuffer(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)

	p.Key("", "h")
	p.Key("", "i")
	if got := p.Model().Input(); got != "hi" {
		t.Fatalf("buffer after two keystrokes = %q, want %q", got, "hi")
	}

	p.Key("backspace", "")
	if got := p.Model().Input(); got != "h" {
		t.Fatalf("buffer after backspace = %q, want %q", got, "h")
	}

	p.Key("esc", "")
	if got := p.Model().Input(); got != "" {
		t.Fatalf("buffer after escape = %q, want empty", got)
	}
}

// TestEnterSubmitsThroughTheModel pins that Enter goes through updateKey like every
// other keystroke. A client that sends its own text would bypass the command mapping
// and the local echo, and the two front ends would drift.
func TestEnterSubmitsThroughTheModel(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Key("", "/join #x")
	p.Key("enter", "")

	if got := p.Model().Input(); got != "" {
		t.Fatalf("the buffer was not cleared by Enter: %q", got)
	}
	// The line is echoed locally, which is how a test can see that Enter submitted
	// rather than merely cleared.
	body := strings.Join(p.Model().Body(80, 20), "\n")
	if !strings.Contains(body, "/join #x") {
		t.Fatalf("Enter did not submit the line; body is %q", body)
	}
}

// TestKeyMessageOverTheSocket pins the wire path, not just the method: the browser
// sends {"type":"key","runes":"x"} and the buffer must grow.
func TestKeyMessageOverTheSocket(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	srv := newWSServer(t, p)
	c, _ := wsDial(t, srv)

	if err := c.WriteJSON(Frame{Type: "key", Runes: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := c.WriteJSON(Frame{Type: "key", Runes: "b"}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.Model().Input() == "ab" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("buffer over the socket = %q, want %q", p.Model().Input(), "ab")
}

// TestClickOnTheNameSelectsTheWindow pins the sidebar bug in the client source. The
// row is built from spans, so a handler reading e.target gets the <span> and finds no
// data-window - only the empty space beside the text worked.
func TestClickOnTheNameSelectsTheWindow(t *testing.T) {
	js := clientJSSource(t)
	if !strings.Contains(js, "closest(\"[data-window]\")") {
		t.Fatal("the window click handler reads e.target directly; clicking the name would do nothing")
	}
	if strings.Contains(js, "t.getAttribute(\"data-window\")") {
		t.Fatal("the window click handler still reads data-window off the target")
	}
}

// TestMobileHasDrawersNotJustHiddenSidebars pins that a phone can still change
// channel. Hiding the sidebars is the obvious mobile fix and it removes the one
// control a chat client must always offer.
func TestMobileHasDrawersNotJustHiddenSidebars(t *testing.T) {
	html := pageHTML(t)
	if !strings.Contains(html, `id="show-windows"`) {
		t.Fatal("no control to open the window list on a small screen")
	}
	if !strings.Contains(html, `id="show-nicks"`) {
		t.Fatal("no control to open the nick list on a small screen")
	}
	css := html
	if !strings.Contains(css, "body.show-windows aside.windows") {
		t.Fatal("the drawer class is not styled, so opening it would do nothing")
	}
	// An input under 16px makes iOS zoom on focus, which is what makes a web chat
	// unusable on a phone.
	if !strings.Contains(css, "font-size: 16px") {
		t.Fatal("the mobile input is smaller than 16px, so iOS will zoom on focus")
	}
}

// TestClientReportsItsViewport pins that the browser tells the server how big it is.
// Without it every phone gets the daemon's default width and every line wraps.
func TestClientReportsItsViewport(t *testing.T) {
	js := clientJSSource(t)
	if !strings.Contains(js, `type: "resize"`) {
		t.Fatal("the client never reports its size")
	}
	if !strings.Contains(js, "ResizeObserver") {
		t.Fatal("the client does not react to its viewport changing")
	}
}

// TestTheBodyFillsTheViewport pins that the conversation is as big as the space it
// was given. A fixed small block in a large window is what a renderer that was never
// told the size looks like, and it is the bug this pins shut.
func TestTheBodyFillsTheViewport(t *testing.T) {
	Configure()
	m := New("#c")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 140, Height: 40})
	for i := 0; i < 50; i++ {
		m, _ = m.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "line"}))
	}

	body := m.Body(m.Width(), m.Height()-3)
	if len(body) != m.Height()-3 {
		t.Fatalf("body is %d lines for a %d-row viewport, want %d", len(body), m.Height(), m.Height()-3)
	}
	for i, line := range body {
		if !strings.Contains(line, "line") {
			continue
		}
		// A rendered line must fit the width it was asked for, not some default.
		if w := len([]rune(stripANSI(line))); w > m.Width() {
			t.Fatalf("line %d is %d columns for a %d-column viewport", i, w, m.Width())
		}
	}
}

// stripANSI removes SGR sequences so a width assertion counts visible characters.
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			for i += 2; i < len(s); i++ {
				if s[i] >= '@' && s[i] <= '~' {
					break
				}
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// TestTheNickListAppearsOnlyOnce pins the duplicate katia saw: the browser has its own
// nick panel, and the old page ALSO embedded the TUI render, which draws its own nick
// column. Every member appeared twice - once as a sidebar and once inside the framed
// terminal.
//
// The fix is that the page embeds Body, which carries no nick column at all: the
// members travel as structured data and the browser draws them where a browser
// expects them. This asserts the body really is free of them, because the duplication
// is invisible to any test that only counts the frame's fields.
func TestTheNickListAppearsOnlyOnce(t *testing.T) {
	Configure()
	p := NewProgram("#c", nil)
	p.Resize(100, 20)
	p.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "hi"}))
	p.Step(Nicklist{Window: "#c", Nicks: []Nick{{Name: "alice", Prefix: "@"}, {Name: "zebediah"}}})

	srv := newWSServer(t, p)
	_, f := wsDial(t, srv)

	body := strings.Join(f.Body, "\n")
	if strings.Contains(body, "zebediah") {
		t.Fatalf("the nick column is inside the conversation, so the browser draws it twice:\n%s", body)
	}
	// The members must still be there as data: the sidebar is the one place they are
	// drawn.
	found := false
	for _, n := range f.Nicks {
		if n.Name == "zebediah" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the nick is nowhere: %+v", f.Nicks)
	}
}

// TestThePageDoesNotEmbedAFrame pins the same thing from the server-rendered page, for
// a browser that has not opened a socket yet: no TUI border, no second input line.
func TestThePageDoesNotEmbedAFrame(t *testing.T) {
	p := NewProgram("#c", nil)
	p.Resize(80, 24)
	p.Step(Incoming(Line{Window: "#c", Nick: "a", Text: "hello"}))

	srv := httptest.NewServer(Handler(p))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	page, _ := io.ReadAll(resp.Body)

	if strings.Contains(string(page), "\u256d") {
		t.Fatal("the page embeds the TUI frame, so the conversation is framed twice")
	}
}
