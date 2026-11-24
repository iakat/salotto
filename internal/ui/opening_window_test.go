package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The TUI used to open on a window named after the user, copied from ZNC's habit of naming a
// client's server window after them. This bouncer does not work that way: it tags every line with
// the window it belongs to, and that window is "status". So the client created a window nothing
// could ever write to, and the daemon's real status lines went to a second one - the sidebar showed
// "1 nicholas" and "2 status", the first permanently empty.
//
// The fix is in cmd/salotto-tui, which a model test cannot reach. What the model can pin is the
// invariant that made the bug visible: the window a client opens on is the window the daemon will
// address, and a fresh client has exactly one window and no unread in it.

// TestStartWindowIsStatusNotTheNick is the rule itself, moved out of cmd/ so it can be tested:
// a rule that lives in a main package is a rule pinned only by whatever runs the binary.
func TestStartWindowIsStatusNotTheNick(t *testing.T) {
	if got := StartWindow(""); got != "status" {
		t.Fatalf("a client with no -window opens on %q, want status", got)
	}
	// An explicit request still wins: someone may deliberately start in a channel.
	if got := StartWindow("#salotto"); got != "#salotto" {
		t.Fatalf("an explicit window became %q", got)
	}
	// And the rule must not quietly depend on a nick: the signature has none, which is the point.
	// A window named after the user cannot receive a line, because the daemon addresses "status".
}

// TestTheOpeningWindowIsTheOneTheDaemonAddresses.
//
// A client that opens on "status" and then receives a line addressed to "status" must still have one
// window, with that line in it. The identity rules already guarantee this - the test is here because
// a front end can defeat them by opening somewhere else, and that is exactly what happened.
func TestTheOpeningWindowIsTheOneTheDaemonAddresses(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	// The daemon's first line: the tagged registration burst, addressed to status.
	for _, text := range []string{"Welcome", "are supported", "End of MOTD"} {
		m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: text, Kind: LineSystem}))
	}

	if len(m.order) != 1 {
		t.Fatalf("the client holds %d windows after the registration burst: %v", len(m.order), m.order)
	}
	if got := len(m.windows[m.order[0]]); got != 3 {
		t.Fatalf("the status window holds %d lines, want 3", got)
	}
	// And nothing is unread: the user is looking at the window those lines arrived in.
	if n := m.unread[m.order[0]]; n != 0 {
		t.Fatalf("%d unread lines in the window the user is looking at", n)
	}
}

// TestAWindowNothingCanAddressIsTheBugItself is the negative control, written as the shape of the
// defect: a client that opens on the nick ends up with two windows and unread in the one it is not
// looking at, which is what the sidebar showed.
func TestAWindowNothingCanAddressIsTheBugItself(t *testing.T) {
	m := New("nicholas") // the old behaviour
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "Welcome", Kind: LineSystem}))

	if len(m.order) != 2 {
		t.Fatalf("expected the old shape to produce two windows, got %d: %v", len(m.order), m.order)
	}
	// The window the client opened on is empty and can never fill.
	if got := len(m.windows[m.order[0]]); got != 0 || winName(m.order[0]) != "nicholas" {
		t.Fatalf("expected an empty window named nicholas first, got %q with %d lines",
			winName(m.order[0]), got)
	}
}
