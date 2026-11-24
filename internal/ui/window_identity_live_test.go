package ui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The sidebar showed "2 status" while the conversation was in #salotto, and our own message was
// drawn there. Two things could produce that, and they are very different bugs: either a window
// called "status" on a second network really exists, or the sidebar is numbering a window that has
// no business existing.

// TestOneStatusWindowStaysOneStatusWindowWhenTagged is the identity case: the client starts on the
// bare window "status", and the first line from the bouncer carries the network tag, which rekeys
// it to "libera/status". That rekeying must move the existing window, not create a second one.
func TestOneStatusWindowStaysOneStatusWindowWhenTagged(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	if len(m.order) != 1 {
		t.Fatalf("a fresh model has %d windows: %v", len(m.order), m.order)
	}

	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "hi", Kind: LineSystem}))

	if len(m.order) != 1 {
		t.Fatalf("one status window became %d: %v", len(m.order), m.order)
	}
	if got := len(m.windows[m.order[0]]); got != 1 {
		t.Fatalf("the rekeyed window holds %d lines, want 1", got)
	}
}

// TestAChannelAndStatusAreTwoWindows pins the ordinary case, so the test above cannot pass by
// collapsing everything into one window.
func TestAChannelAndStatusAreTwoWindows(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "hi", Kind: LineSystem}))
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "libera", Nick: "alice", Text: "yo", Kind: LineMessage}))

	if len(m.order) != 2 {
		t.Fatalf("%d windows, want 2: %v", len(m.order), m.order)
	}
}

// TestOurOwnLineStaysInTheChannelItWasSentTo.
//
// The echo of a line we sent must land in the window it was addressed to. The daemon echoes with
// our joined nick as the source and the channel as the target; the client draws its own copy too.
// If the relayed echo is routed by anything other than the target, it lands somewhere else and the
// user sees their message in a window they were not writing to.
func TestOurOwnLineStaysInTheChannelItWasSentTo(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	// Join, so the client knows the channel exists and that we are in it.
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "libera", Nick: "nicholas", Text: "joined", Kind: LineSystem}))

	// The daemon's echo: our own nick, the channel as target.
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "libera", Nick: "nicholas", Text: "hi there", Kind: LineMessage}))

	status := m.keyFor("status")
	for _, line := range m.windows[status] {
		if line.Text == "hi there" {
			t.Fatalf("our own line was drawn in the status window as well as the channel")
		}
	}
	found := false
	for _, line := range m.windows[m.keyFor("#salotto")] {
		if line.Text == "hi there" {
			found = true
		}
	}
	if !found {
		t.Fatal("the echo of our own line never reached the channel")
	}
}
