package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// A command typed into the input used to be echoed into the window as a line of our own, alongside
// the server's own answer to it. Live, "/topic a new topic" appeared as
//
//	<me> /topic a new topic
//	set the topic: a new topic
//	set the topic: a new topic
//
// - the command drawn as if it were conversation, and the effect reported twice, because the
// client's echo is confirmed by the relayed line that says the same thing.
//
// The unit tests missed it because they type commands the model resolves itself (/go, /search,
// /fset), which return before reaching the echo. Only a command that goes to the wire reaches it.

// TestAWireCommandIsNotEchoedAsAMessage is the defect.
func TestAWireCommandIsNotEchoedAsAMessage(t *testing.T) {
	m := connectedModel()
	before := len(m.windows[m.active])

	m, _ = m.Step(Sent{Text: "/topic a new topic"})

	if got := len(m.windows[m.active]); got != before {
		t.Fatalf("a wire command added %d lines to the window; it must be sent, not drawn",
			got-before)
	}
	// And it really did go to the wire: silence would also leave the window unchanged.
	cmd := typeLineCmd(connectedModel(), "/topic a new topic")
	if cmd == nil {
		t.Fatal("the command produced nothing for the wire")
	}
	msg, ok := cmd().(Sent)
	if !ok {
		t.Fatalf("the command produced %T, want Sent", cmd())
	}
	if !strings.HasPrefix(msg.Text, "/topic") {
		t.Fatalf("the command sent %q", msg.Text)
	}
}

// TestAPlainMessageIsStillEchoed is the negative control: the fix must not swallow conversation.
func TestAPlainMessageIsStillEchoed(t *testing.T) {
	m := connectedModel()
	before := len(m.windows[m.active])
	// The echo happens on the Sent message the driver feeds back after sending, not on the
	// keystroke: typeLine discards the command, so a test that only types sees nothing.
	m, _ = m.Step(Sent{Text: "hello there"})

	if got := len(m.windows[m.active]); got != before+1 {
		t.Fatalf("a message added %d lines, want 1", got-before)
	}
	last := m.windows[m.active][len(m.windows[m.active])-1]
	if last.Text != "hello there" || last.Nick != "me" || !last.Pending {
		t.Fatalf("the echo is %+v", last)
	}
}

// TestAnActionIsStillEchoed pins the exception: /me is a command syntactically and a message in
// fact. It goes out as a CTCP ACTION and comes back, so it has to be drawn optimistically like any
// other line or the user waits a round trip to see what they just said.
func TestAnActionIsStillEchoed(t *testing.T) {
	m := connectedModel()
	before := len(m.windows[m.active])
	m, _ = m.Step(Sent{Text: "/me waves"})

	if got := len(m.windows[m.active]); got != before+1 {
		t.Fatalf("an action added %d lines, want 1", got-before)
	}
	last := m.windows[m.active][len(m.windows[m.active])-1]
	if last.Kind != LineAction {
		t.Fatalf("the action echo has kind %v, want LineAction", last.Kind)
	}
	// The echo carries the body, not the command: the relayed line will say "waves".
	if last.Text != "waves" {
		t.Fatalf("the action echo says %q, want the body", last.Text)
	}
}

// connectedModel is a model that believes it is connected, so the not-connected guard does not
// stand in front of the echo path.
func connectedModel() Model {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.connected = true
	return m
}
