package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The reconnect tests in this file exist because of one measured number: before the duplicate
// check, feeding three lines, dropping the connection and replaying the same three left the
// window holding six. Reconnect without deduplication is worse than no reconnect, so the two
// are tested together.

// replayModel builds a model that has already seen lines and is now in a replay window, which
// is the state the transport puts it in after a drop.
func replayModel(t *testing.T) Model {
	t.Helper()
	m := New("status")
	m.connected = true
	return m
}

// step is tea's Update for the model alone: most tests here care about the model, not the
// command, and a discarded tea.Cmd is the usual shape in this package.
func step(t *testing.T, m Model, msg tea.Msg) Model {
	t.Helper()
	next, _ := m.Step(msg)
	return next
}

// TestAReplayedLineIsNotDrawnTwice is the bug itself: same bytes, same window, twice.
func TestAReplayedLineIsNotDrawnTwice(t *testing.T) {
	m := replayModel(t)
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

	first := Line{Window: "#salotto", Nick: "alice", Text: "hello", Kind: LineMessage, Time: at}
	m = step(t, m, Incoming(first))

	if got := lineCount(m, "#salotto"); got != 1 {
		t.Fatalf("after the live line the window holds %d line(s), want 1", got)
	}

	// The connection drops and comes back: the bouncer replays the ring, which is the same
	// line with the same timestamp, because the daemon stamped it when it buffered it.
	m = step(t, m, Disconnected{})
	m = step(t, m, Reconnecting{})

	m = step(t, m, Incoming(first))
	if got := lineCount(m, "#salotto"); got != 1 {
		t.Fatalf("a replayed line was drawn again: the window holds %d line(s), want 1", got)
	}

	// And a line that arrives after the replay is still drawn: suppression must not leak into
	// live traffic, or the client would silently eat messages.
	m = step(t, m, ReplayFinished{})
	m = step(t, m, Incoming(Line{Window: "#salotto", Nick: "alice", Text: "hello", Kind: LineMessage, Time: at.Add(time.Second)}))
	if got := lineCount(m, "#salotto"); got != 2 {
		t.Fatalf("a live line after the replay was dropped: the window holds %d line(s), want 2", got)
	}
}

// TestALiveLineIsNeverDeduplicated.
//
// The dangerous version of this feature compares every line, not just replayed ones. Then two
// people saying "ok" in the same second - or one person saying it twice - loses a line, and
// the loss is silent. This is the negative control.
func TestALiveLineIsNeverDeduplicated(t *testing.T) {
	m := replayModel(t)
	// No Disconnected/Reconnecting: this is a normal session.
	ln := Line{Window: "#salotto", Nick: "alice", Text: "ok", Kind: LineMessage, Time: time.Now()}
	m = step(t, m, Incoming(ln))
	m = step(t, m, Incoming(ln))
	if got := lineCount(m, "#salotto"); got != 2 {
		t.Fatalf("two identical live lines collapsed into %d; a live line is new by definition", got)
	}
}

// TestATimestampIsNotPartOfALineIdentity.
//
// This is the bug the live probe against the real daemon found, and it would have shipped
// silently. A live line's time tag is stamped when the bouncer fans it out; a replayed line's
// comes out of the ring, where it was stamped when it was buffered. Two clock reads, one
// millisecond apart - measured: 17.242 live, 17.241 out of the ring. A comparison that includes
// the timestamp therefore never matches, and the replay doubles the scrollback exactly as before
// the deduplication existed.
func TestATimestampIsNotPartOfALineIdentity(t *testing.T) {
	m := replayModel(t)
	at := time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

	m = step(t, m, Incoming(Line{Window: "#salotto", Nick: "alice", Text: "hello", Kind: LineMessage, Time: at}))
	m = step(t, m, Disconnected{})
	m = step(t, m, Reconnecting{})

	// The replay carries the ring's stamp, which is a millisecond earlier than the fan-out
	// stamp the first copy got.
	m = step(t, m, Incoming(Line{Window: "#salotto", Nick: "alice", Text: "hello", Kind: LineMessage, Time: at.Add(-time.Millisecond)}))
	if n := lineCount(m, "#salotto"); n != 1 {
		t.Fatalf("the window holds %d line(s); a replay with the ring's timestamp was drawn again", n)
	}
}

// TestAReplayNeverEatsARealRepeat.
//
// The client was away while alice said "ok" three times and it saw it twice. Replaying two
// copies must not swallow the third: a multiset, not a set. Dropping a message the user never
// saw is worse than repeating one they did.
func TestAReplayNeverEatsARealRepeat(t *testing.T) {
	m := replayModel(t)
	ln := func(i int) Line {
		return Line{Window: "#salotto", Nick: "alice", Text: "ok", Kind: LineMessage, Time: time.Unix(int64(i), 0)}
	}
	m = step(t, m, Incoming(ln(1)))
	m = step(t, m, Incoming(ln(2)))
	m = step(t, m, Disconnected{})
	m = step(t, m, Reconnecting{})

	// Three copies arrive; the client held two, so exactly one is new.
	for i := 1; i <= 3; i++ {
		m = step(t, m, Incoming(ln(i*10))) // different timestamps: the ring restamped them
	}
	if n := lineCount(m, "#salotto"); n != 3 {
		t.Fatalf("the window holds %d line(s) after replaying three copies over two held, want 3", n)
	}
}

// TestAReplayOfADifferentLineIsKept: the check is exact, not "something like this is here".
func TestAReplayOfADifferentLineIsKept(t *testing.T) {
	m := replayModel(t)
	at := time.Now()
	m = step(t, m, Incoming(Line{Window: "#salotto", Nick: "alice", Text: "hello", Kind: LineMessage, Time: at}))
	m = step(t, m, Disconnected{})
	m = step(t, m, Reconnecting{})

	cases := []struct {
		name string
		line Line
	}{
		{"same text from another nick", Line{Window: "#salotto", Nick: "bob", Text: "hello", Kind: LineMessage, Time: at}},
		{"same text in another window", Line{Window: "#other", Nick: "alice", Text: "hello", Kind: LineMessage, Time: at}},
		{"same text as another kind", Line{Window: "#salotto", Nick: "alice", Text: "hello", Kind: LineAction, Time: at}},
	}
	// The count is read before the loop and the subtest asks "does this line land in the
	// window at all", not "is the window one longer than before": a Step returns a Model that
	// shares the line map, so comparing two live Models after the fact compares a map with
	// itself. The question that matters here is the branch, not the arithmetic.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := step(t, m, Incoming(tc.line))
			if !holdsLine(got, "#salotto", tc.line) {
				t.Fatalf("a replayed line that differs was dropped as a duplicate")
			}
		})
	}
}

// TestTheReplayWindowClosesOnEndOfRegistration.
//
// If ReplayFinished did nothing, the duplicate check would stay on for the life of the
// connection and eat live lines. If it closed on the first line instead, a slow replay would
// be cut short and its tail duplicated.
func TestTheReplayWindowClosesOnEndOfRegistration(t *testing.T) {
	m := replayModel(t)
	at := time.Now()
	ln := Line{Window: "#salotto", Nick: "alice", Text: "hi", Kind: LineMessage, Time: at}
	m = step(t, m, Incoming(ln))
	m = step(t, m, Reconnecting{})
	if !m.replaying {
		t.Fatal("a reconnect did not open the replay window")
	}
	m = step(t, m, ReplayFinished{})
	if m.replaying {
		t.Fatal("end of registration did not close the replay window")
	}
	// After it closes, an identical line is drawn: it is a real repeat, not a replay.
	m = step(t, m, Incoming(ln))
	if got := lineCount(m, "#salotto"); got != 2 {
		t.Fatalf("the window holds %d line(s) after a genuine repeat, want 2", got)
	}
}

// TestReconnectStateIsVisibleButNotNoisy.
//
// A bouncer that is down for a minute is a handful of attempts. One line per attempt would
// bury the reason; zero lines would look like the client is fine.
func TestReconnectStateIsVisibleButNotNoisy(t *testing.T) {
	m := replayModel(t)
	m = step(t, m, Disconnected{})
	if m.connected {
		t.Fatal("a disconnect did not mark the model offline")
	}
	m = step(t, m, Reconnecting{})
	m = step(t, m, Reconnecting{})
	texts := textsOf(m, "status")
	n := 0
	for _, s := range texts {
		if s == "reconnecting..." {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d reconnecting notice(s) in status, want exactly 1", n)
	}

	// 001 puts it back: without this the client would stay marked offline forever.
	m = step(t, m, Registered{})
	if !m.connected {
		t.Fatal("001 did not mark the model connected again")
	}
}

// TestAFailedAttemptIsReportedOncePerMinute: a refused socket repeated every two seconds is
// the same sentence six times a minute in the status window.
func TestAFailedAttemptIsReportedOncePerMinute(t *testing.T) {
	m := replayModel(t)
	m = step(t, m, Disconnected{})
	m = step(t, m, ReconnectFailed{Attempt: 1, Err: errStub("connection refused")})

	// The second failure is inside the minute, so it must stay quiet. The third is an hour
	// later, so it must be said: "silence forever" would hide a bouncer that never came back.
	m = step(t, m, ReconnectFailed{Attempt: 2, Err: errStub("connection refused")})
	if n := failureNotices(m); n != 1 {
		t.Fatalf("%d failure notice(s) inside the same minute, want 1", n)
	}
	m.reconnectAt = time.Now().Add(-2 * time.Minute)
	m = step(t, m, ReconnectFailed{Attempt: 3, Err: errStub("connection refused")})
	if n := failureNotices(m); n != 2 {
		t.Fatalf("%d failure notice(s) after a minute, want 2", n)
	}
}

// errStub is a bare error with a fixed message, so a test does not depend on net's wording.
type errStub string

func (e errStub) Error() string { return string(e) }

// textsOf is the text of every line the window holds, which is what these tests assert on:
// the kind and the time matter for the duplicate check, but a human reading the screen only
// sees the text.
func textsOf(m Model, window string) []string {
	var out []string
	for _, l := range m.windows[m.keyFor(window)] {
		out = append(out, l.Text)
	}
	return out
}

// holdsLine reports whether the window contains this exact line, which is the observable
// difference between "kept" and "dropped as a replay".
func holdsLine(m Model, window string, want Line) bool {
	for _, l := range m.windows[m.keyFor(window)] {
		if l.Nick == want.Nick && l.Text == want.Text && l.Kind == want.Kind && l.Time.Equal(want.Time) {
			return true
		}
	}
	return false
}

// lineCount is how many lines a window holds.
func lineCount(m Model, window string) int {
	return len(m.windows[m.keyFor(window)])
}

// failureNotices counts the status lines that report a failed attempt.
func failureNotices(m Model) int {
	n := 0
	for _, s := range textsOf(m, "status") {
		if strings.HasPrefix(s, "connection failed") {
			n++
		}
	}
	return n
}

// TestTheHandshakeIsRepeatedOnTheNewConnection.
//
// A socket that reconnects but never sends NICK is a connection with no identity: the bouncer
// would have nothing to attach a client to and the user would see "reconnecting..." forever.
// The credentials are kept from the first Register for exactly this.
func TestTheHandshakeIsRepeatedOnTheNewConnection(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	tr.Register("alice", "sekrit")

	// CAP LS comes first: registration is the daemon's own handshake, and what matters here
	// is that the credentials are among the lines, not their position in the burst.
	if got := fb.handshake(t); got != "PASS sekrit" {
		t.Fatalf("first connection got %q, want PASS", got)
	}
	if got := fb.handshake(t); got != "NICK alice" {
		t.Fatalf("first connection got %q, want NICK", got)
	}

	// Everything the drop triggers arrives on the stream, and then the new connection repeats
	// the handshake on its own.
	_ = fb.client(t).Close()
	deadline := time.After(6 * time.Second)
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("the stream closed instead of reconnecting")
			}
			if _, isRe := msg.(Reconnecting); isRe {
				goto registered
			}
		case <-deadline:
			t.Fatal("no reconnect, so the handshake was never repeated")
		}
	}

registered:
	// The credentials arrive again on the second connection. Their position in the burst is
	// the daemon's business; their presence is what a reconnect without them would lose.
	if got := fb.handshake(t); got != "PASS sekrit" {
		t.Fatalf("the new connection got %q, want PASS again", got)
	}
	if got := fb.handshake(t); got != "NICK alice" {
		t.Fatalf("the new connection got %q, want NICK again", got)
	}
}

// TestAnAttemptThatCannotOpenASocketIsAnnounced.
//
// "reconnecting..." alone says the client is trying; it does not say the socket was refused,
// which is the one thing that suggests the bouncer is not running at all. Closing the listener
// is how a dead bouncer looks from here.
func TestAnAttemptThatCannotOpenASocketIsAnnounced(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	// Both ends go: the listener so the next dial is refused, and the live connection so the
	// reader notices at all. Closing only the listener would leave the transport reading a
	// socket that is still fine, and there would be no attempt to report.
	_ = fb.client(t).Close()
	fb.ln.Close()

	deadline := time.After(10 * time.Second)
	var sawFail bool
	for !sawFail {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("the stream closed instead of retrying against a dead address")
			}
			if rf, isFail := msg.(ReconnectFailed); isFail {
				if rf.Err == nil {
					t.Fatal("a failed attempt carried no reason")
				}
				sawFail = true
			}
		case <-deadline:
			t.Fatal("a refused attempt was never reported")
		}
	}
}
