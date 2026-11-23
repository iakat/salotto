package ui

import (
	"bufio"
	"net"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeBouncer is a listener that records what the transport sends and lets a test
// inject lines, so the mappings can be exercised without a real bouncer.
type fakeBouncer struct {
	ln   net.Listener
	conn chan net.Conn // buffered(1): the accepted connection, published once
	sent chan string

	// cached is the connection once client() has taken it off the channel, so
	// repeated calls return the same conn instead of blocking on a drained
	// channel. It is only touched from the test goroutine.
	cached net.Conn
}

func newFakeBouncer(t *testing.T) *fakeBouncer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fb := &fakeBouncer{
		ln:   ln,
		conn: make(chan net.Conn, 1),
		sent: make(chan string, 32),
	}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		fb.conn <- conn
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				fb.sent <- strings.TrimRight(line, "\r\n")
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return fb
}

// client returns the accepted connection, waiting for the accept if needed. The
// channel is the only synchronisation the helper needs, so -race is quiet.
func (fb *fakeBouncer) client(t *testing.T) net.Conn {
	t.Helper()
	if fb.cached != nil {
		return fb.cached
	}
	select {
	case conn := <-fb.conn:
		fb.cached = conn
		return conn
	case <-time.After(3 * time.Second):
		t.Fatal("transport never connected")
		return nil
	}
}

func (fb *fakeBouncer) inject(t *testing.T, line string) {
	t.Helper()
	conn := fb.client(t)
	if _, err := conn.Write([]byte(line + "\r\n")); err != nil {
		t.Fatal(err)
	}
}

func (fb *fakeBouncer) expect(t *testing.T) string {
	t.Helper()
	select {
	case line := <-fb.sent:
		return line
	case <-time.After(3 * time.Second):
		t.Fatal("nothing was sent to the bouncer")
		return ""
	}
}

// collectIncoming reads model messages until it has n Incoming ones.
func collectIncoming(t *testing.T, tr *Transport, n int) []Line {
	t.Helper()
	var out []Line
	deadline := time.After(3 * time.Second)
	for len(out) < n {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatalf("transport closed early, got %d of %d", len(out), n)
			}
			if in, ok := msg.(Incoming); ok {
				out = append(out, Line(in))
			}
		case <-deadline:
			t.Fatalf("timed out with %d of %d lines", len(out), n)
		}
	}
	return out
}

// TestTransportRoutesChannelLines: a channel message lands in the channel's
// window, not in status, which is the whole reason the transport exists.
func TestTransportRoutesChannelLines(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":alice!a@h PRIVMSG #salotto :hello there")
	fb.inject(t, ":bob!b@h PRIVMSG #other :elsewhere")

	got := collectIncoming(t, tr, 2)
	if got[0].Window != "#salotto" || got[0].Nick != "alice" || got[0].Text != "hello there" {
		t.Fatalf("channel line: %+v", got[0])
	}
	if got[1].Window != "#other" {
		t.Fatalf("second channel line routed to %q", got[1].Window)
	}
}

// TestTransportRoutesPrivateMessagesToTheSender: a PM opens a query window named
// after the sender, which is what every IRC client does and what the model's
// window list expects.
func TestTransportRoutesPrivateMessagesToTheSender(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":carol!c@h PRIVMSG me :a private word")
	got := collectIncoming(t, tr, 1)
	if got[0].Window != "carol" || got[0].Text != "a private word" {
		t.Fatalf("private message: %+v", got[0])
	}
}

// TestTransportRendersActions: a CTCP ACTION becomes LineAction with the wrapper
// stripped, so the view can style it as an action instead of showing \x01.
func TestTransportRendersActions(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":dave!d@h PRIVMSG #salotto :\x01ACTION waves\x01")
	got := collectIncoming(t, tr, 1)
	if got[0].Kind != LineAction || got[0].Text != "waves" {
		t.Fatalf("action: %+v", got[0])
	}
	if strings.ContainsAny(got[0].Text, "\x01") {
		t.Fatalf("CTCP wrapper leaked into the text: %q", got[0].Text)
	}
}

// TestTransportDropsOtherCTCPs pins that VERSION and friends do not appear as
// chat: a CTCP that is not an ACTION is protocol noise, not conversation.
func TestTransportDropsOtherCTCPs(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":eve!e@h PRIVMSG #salotto :\x01VERSION\x01")
	fb.inject(t, ":eve!e@h PRIVMSG #salotto :after the ctcp")

	got := collectIncoming(t, tr, 1)
	if got[0].Text != "after the ctcp" {
		t.Fatalf("CTCP was rendered as chat: %+v", got)
	}
}

// TestTransportStatusNotices: a NOTICE from the bouncer itself goes to status,
// which is where a user looks for its answers.
func TestTransportStatusNotices(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":status!status@salotto NOTICE me :Server added")
	got := collectIncoming(t, tr, 1)
	if got[0].Window != "status" || got[0].Kind != LineNotice || got[0].Text != "Server added" {
		t.Fatalf("status notice: %+v", got[0])
	}
}

// TestTransportAnswersPing: a direct connection to an IRCd is PINGed, and the
// transport must answer or the link dies.
func TestTransportAnswersPing(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, "PING :abc123")
	if got := fb.expect(t); got != "PONG :abc123" {
		t.Fatalf("ping answer: %q", got)
	}
}

// TestTransportCommandMapping pins the "/" commands: the ones that are real IRC
// verbs go out as verbs, /msg becomes a PRIVMSG, /me becomes an ACTION, and
// anything else is a salotto bouncer command sent as a bare verb.
func TestTransportCommandMapping(t *testing.T) {
	tr := &Transport{windows: map[string]bool{}, active: "#salotto"}
	cases := []struct{ in, want string }{
		{"hello", "PRIVMSG #salotto :hello"},
		{"/me waves", "PRIVMSG #salotto :\x01ACTION waves\x01"},
		{"/join #new", "JOIN #new"},
		{"/msg alice hi there", "PRIVMSG alice :hi there"},
		{"/playbuffer #salotto", "PLAYBUFFER #salotto"},
		{"/listchans", "LISTCHANS "},
		{"/whois alice", "WHOIS alice"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := tr.Command(tc.in); got != strings.TrimRight(tc.want, " ") {
			t.Fatalf("Command(%q) = %q, want %q", tc.in, got, strings.TrimRight(tc.want, " "))
		}
	}

	// In the status window a bare line is a bouncer command, not a PRIVMSG to
	// a window called "status" — there is nobody to send it to.
	tr.active = "status"
	if got := tr.Command("listnetworks"); got != "listnetworks" {
		t.Fatalf("status window bare line = %q", got)
	}
}

// TestTransportFeedsTheModel is the actual integration: lines from the wire reach
// View(). This is the test that would have failed before Transport existed, since
// the model had nothing feeding it.
func TestTransportFeedsTheModel(t *testing.T) {
	Configure()
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	m := New("#salotto")
	fb.inject(t, ":alice!a@h PRIVMSG #salotto :hello from the wire")

	deadline := time.After(3 * time.Second)
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				t.Fatal("transport closed before delivering the line")
			}
			m, _ = m.Step(msg)
			if strings.Contains(Plain(m.View()), "hello from the wire") {
				return
			}
		case <-deadline:
			t.Fatalf("view never showed the line: %q", Plain(m.View()))
		}
	}
}

// TestTransportClosesOnDisconnect: the message channel must close so a Program
// can exit rather than hang on a dead link.
func TestTransportClosesOnDisconnect(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the accept, then drop the connection from the far side.
	fb.inject(t, ":alice!a@h PRIVMSG #x :hi")
	collectIncoming(t, tr, 1)
	_ = fb.client(t).Close()

	deadline := time.After(3 * time.Second)
	for {
		select {
		case _, ok := <-tr.Msgs():
			if !ok {
				if tr.Err() == nil {
					t.Fatal("closed without recording an error")
				}
				return
			}
		case <-deadline:
			t.Fatal("message channel never closed after the peer hung up")
		}
	}
}

// TestQueuedMessagesAreDrainedAsTeaMsgs guards the shape a Program depends on:
// every message on the channel is a tea.Msg the model can Step.
func TestQueuedMessagesAreDrainedAsTeaMsgs(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":alice!a@h PRIVMSG #x :one")
	var msg tea.Msg
	select {
	case msg = <-tr.Msgs():
	case <-time.After(3 * time.Second):
		t.Fatal("no message")
	}
	if _, ok := msg.(Incoming); !ok {
		t.Fatalf("message is %T, want Incoming", msg)
	}
}

// TestTransportOpensAWindowOnOurOwnJoin pins the routing rule that made the web
// front end's /join look broken.
//
// A JOIN was always a status line. That is right for other people's joins, but
// our OWN join is what creates the window: if it goes to status, the channel
// exists in the bouncer and the UI never learns about it, so the sidebar stays at
// one entry and a typed line has nowhere to go. The bouncer sends the JOIN only
// after it accepted ours, so this line is the confirmation that the window is real.
func TestTransportOpensAWindowOnOurOwnJoin(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	// 001 first: without knowing our own nick, no JOIN can be attributed to us.
	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":me!m@h JOIN #fresh")
	// And somebody else's join must still be status, not a window.
	fb.inject(t, ":someone!s@h JOIN #someoneelse")

	got := collectIncoming(t, tr, 3)
	if got[0].Window != "status" {
		t.Fatalf("001 routed to %q", got[0].Window)
	}
	if got[1].Window != "#fresh" {
		t.Fatalf("our own JOIN routed to %q, want #fresh (the window never opens)", got[1].Window)
	}
	if got[2].Window != "status" {
		t.Fatalf("another user's JOIN opened window %q", got[2].Window)
	}
}

// TestTransportExtendedJoinOpensTheWindow is the same rule for the extended-join
// form, which the IRCd sends when the client negotiated the capability:
// ":me JOIN #chan account :realname". The channel is still the first parameter,
// but it arrives with a leading colon and is followed by more fields, so a naive
// split on the whole parameter string would name the window "#chan account".
func TestTransportExtendedJoinOpensTheWindow(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	fb.inject(t, ":irc.test 001 me :welcome")
	fb.inject(t, ":me!m@h JOIN :#ext account :Real Name")

	got := collectIncoming(t, tr, 2)
	if got[1].Window != "#ext" {
		t.Fatalf("extended join routed to %q", got[1].Window)
	}
}
