package ui

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// A dropped connection has to be said out loud.
//
// The transport's channel closing is the driver's signal to stop pumping and carries nothing
// else, so before this the model was never told: the last frame stayed on screen with the
// window list, the nicklist and the topic as they were, still reading "connected" because that
// flag was set once at New() and never revised. A user typing into that screen was typing into
// nothing - the line appeared in the window, the echo looked optimistic and pending, and no
// error came, because no one was there to send it.

// deadLink returns a transport attached to a listener that closes the connection at once, which
// is what a bouncer restart looks like from here.
func deadLink(t *testing.T) *Transport {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		c.Close()
	}()
	tr, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tr.Close() })
	return tr
}

// drainUntilDisconnected reads until the transport reports a drop, then stops. It does not
// wait for the channel to close: the transport retries after a drop, so a closed channel now
// means "the client was closed", not "the connection failed".
func drainUntilDisconnected(t *testing.T, tr *Transport) []tea.Msg {
	t.Helper()
	var got []tea.Msg
	deadline := time.After(5 * time.Second)
	for {
		select {
		case msg, ok := <-tr.Msgs():
			if !ok {
				return got
			}
			got = append(got, msg)
			if _, isDisc := msg.(Disconnected); isDisc {
				return got
			}
		case <-deadline:
			t.Fatalf("no disconnect reported after %d message(s)", len(got))
		}
	}
}

// TestADroppedConnectionSendsDisconnected.
func TestADroppedConnectionSendsDisconnected(t *testing.T) {
	tr := deadLink(t)
	got := drainUntilDisconnected(t, tr)

	var disc *Disconnected
	for _, msg := range got {
		if d, ok := msg.(Disconnected); ok {
			disc = &d
		}
	}
	if disc == nil {
		t.Fatalf("the stream closed without a Disconnected message; the model would keep drawing a live connection: %v", got)
	}
	if disc.Reason == nil {
		t.Fatalf("Disconnected carries no reason, so the user cannot tell a restart from a network failure")
	}
}

// TestAReasonIsNotInventedWhenWeClosedIt: our own Close is not news. Reporting it would put "use
// of closed network connection" in the status window every time the user quits.
func TestAReasonIsNotInventedWhenWeClosedIt(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	held := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			held <- c // keep it open: the client is the one closing
		}
	}()

	tr, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the accept land
	tr.Close()

	select {
	case msg, ok := <-tr.Msgs():
		if ok {
			if _, isDisc := msg.(Disconnected); isDisc {
				t.Fatalf("a Close of our own was reported as a disconnect")
			}
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("the stream did not close")
	}
}

// TestTheModelAdmitsItIsDisconnected: the message alone is not enough; the screen has to change.
func TestTheModelAdmitsItIsDisconnected(t *testing.T) {
	m := New("status")
	if !m.Connected() {
		t.Fatalf("a fresh model should start connected")
	}
	next, _ := m.Update(Disconnected{Reason: errors.New("EOF")})
	m2 := next.(Model)

	if m2.Connected() {
		t.Fatalf("the model still reports connected after Disconnected")
	}
	var said []string
	for _, l := range m2.windows["status"] {
		said = append(said, l.Text)
	}
	joined := strings.Join(said, "\n")
	if !strings.Contains(joined, "ended") {
		t.Fatalf("nothing in the status window says the connection ended: %q", joined)
	}
	// The reason is shown, because "the bouncer restarted" and "the network is down" are
	// different problems and only the transport knows which happened.
	if !strings.Contains(joined, "EOF") {
		t.Fatalf("the reason was dropped: %q", joined)
	}
	// Drawn as an error, not as grey system text: this is the one line in the window the user
	// has to act on.
	for _, l := range m2.windows["status"] {
		if strings.Contains(l.Text, "EOF") && l.Kind != LineError {
			t.Fatalf("the disconnect notice has kind %v, want LineError", l.Kind)
		}
	}
	// And the status bar says so, because it is what the user glances at.
	bar := plainText(m2.statusBar())
	if !strings.Contains(bar, "offline") {
		t.Fatalf("the status bar still claims to be connected: %q", bar)
	}
}

// TestTheModelRefusesToSendWhenDisconnected.
//
// The transport cannot deliver a report once its stream is closed, so the refusal has to happen
// in the model, where the user is looking. Echoing the line there and letting it hang as pending
// is the worst outcome: it looks sent, so the user waits for an answer to something that never
// left the machine.
func TestTheModelRefusesToSendWhenDisconnected(t *testing.T) {
	m := New("#test")
	next, _ := m.Update(Disconnected{Reason: errors.New("EOF")})
	m2 := next.(Model)

	submitted, cmd := m2.Update(Sent{Text: "hello there"})
	m3 := submitted.(Model)
	if cmd != nil {
		t.Fatalf("a disconnected model still produced a command to send")
	}
	var lines []Line
	for _, l := range m3.windows["#test"] {
		lines = append(lines, l)
	}
	if len(lines) != 1 {
		t.Fatalf("typing on a dead connection produced %d line(s), want one refusal", len(lines))
	}
	if lines[0].Kind != LineError {
		t.Fatalf("the refusal is drawn as %v, want LineError", lines[0].Kind)
	}
	if !strings.Contains(lines[0].Text, "not connected") {
		t.Fatalf("the refusal does not say why: %q", lines[0].Text)
	}
	if lines[0].Pending {
		t.Fatalf("the refusal is marked pending, which is the ghost it is meant to prevent")
	}
	// The input is cleared, because the text is now in the window and leaving it in the box
	// would suggest it is still waiting to be sent.
	if len(m3.input) != 0 {
		t.Fatalf("the input still holds %q after a refusal", string(m3.input))
	}
}

// TestSendingOnADeadConnectionSaysSo.
//
// push sent on t.msgs after readLoop had closed it, so the first keystroke after a bouncer
// restart panicked the client - and the moment it happens is exactly when someone is typing,
// because the screen has not caught up with the connection yet.
func TestSendingOnADeadConnectionDoesNotPanic(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			held <- c
		}
	}()
	tr, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	tr.Close()
	select {
	case c := <-held:
		c.Close()
	case <-time.After(2 * time.Second):
	}
	ln.Close()

	// After Close the transport is stopped for good, which is the state a line is refused in.
	tr.Send("PRIVMSG #test :hello")

	// No assertion on the stream: it is closed, so reading it returns the zero value forever
	// and says nothing. What matters is that the call below returned rather than panicking
	// with "send on closed channel". The user-facing half is the model's - see
	// TestTheModelRefusesToSendWhenDisconnected.

	// A second Send must also be safe: a user who does not notice and tries again.
	tr.Send("PRIVMSG #test :hello again")
}
