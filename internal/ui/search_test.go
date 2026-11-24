package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestSearchCommandFindsAcrossWindows is the feature W4 claimed and did not have.
//
// Model.Search existed with no caller: no command reached it and the browser never asked. A dead
// method is not a feature, and the queue entry saying "in-client half done" was wrong. This drives
// the command the way a user does and reads the window.
func TestSearchCommandFindsAcrossWindows(t *testing.T) {
	m := searchFixture(t)

	m = typeLine(m, "/search spec")
	body := systemText(m)
	if !strings.Contains(body, "match") {
		t.Fatalf("no count reported: %q", body)
	}
	// The hit must name the window it is in, or the user cannot go back to it.
	if !strings.Contains(body, "#salotto") {
		t.Fatalf("the hit does not name its window: %q", body)
	}
	// And the text of the line.
	if !strings.Contains(body, "spec again") {
		t.Fatalf("the matched text is missing: %q", body)
	}
}

// TestSearchCrossesEveryWindow pins the "cross-channel" half of W4: a hit in a window the user is
// not looking at is found and labelled.
func TestSearchCrossesEveryWindow(t *testing.T) {
	m := searchFixture(t)

	m = typeLine(m, "/search needle")
	body := systemText(m)
	if !strings.Contains(body, "#salotto") {
		t.Fatalf("the hit in #salotto is missing: %q", body)
	}
	if !strings.Contains(body, "#other") {
		t.Fatalf("the hit in the other window is missing: %q", body)
	}
}

// TestSearchWithNoMatchesSaysWhatItSearchedFor pins the message: "nothing found" without the query
// is a message a user cannot act on, because they may have typo'd and may not remember what they
// typed three keystrokes ago.
func TestSearchWithNoMatchesSaysWhatItSearchedFor(t *testing.T) {
	m := searchFixture(t)

	m = typeLine(m, "/search zzzznothing")
	body := systemText(m)
	if !strings.Contains(body, "zzzznothing") {
		t.Fatalf("the empty result did not name the query: %q", body)
	}
	if strings.Contains(body, "match(es)") {
		t.Fatalf("an empty search reported matches: %q", body)
	}
}

// TestSearchIsNotEmpty pins "no answer" as a bug: /search with no argument is a usage message, not
// a search for the empty string, which would match every line ever shown.
func TestSearchIsNotEmpty(t *testing.T) {
	m := searchFixture(t)

	m = typeLine(m, "/search")
	body := systemText(m)
	if !strings.Contains(body, "usage") {
		t.Fatalf("/search alone said %q", body)
	}
	if strings.Contains(body, "match(es)") {
		t.Fatalf("an empty /search ran a search: %q", body)
	}
}

// TestSearchDoesNotGoToTheWire pins that the command is resolved in the model. The scrollback is
// the client's; sending SEARCH upstream would return the bouncer's ring instead of what this client
// has shown, which is a different and smaller thing.
func TestSearchDoesNotGoToTheWire(t *testing.T) {
	// The command the enter produced is what would be sent; the model resolves /search itself, so
	// there must be nothing to send. This is the same instrument the /go tests use.
	if cmd := typeLineCmd(searchFixture(t), "/search salotto"); cmd != nil {
		t.Fatal("the search produced a command for the wire; it must be resolved in the model")
	}
}

// TestASearchIsCapped pins that a one-word query cannot flood the window and push out the
// conversation the user was reading.
func TestASearchIsCapped(t *testing.T) {
	m := searchFixture(t)
	// Stuff the active window with matches.
	for i := 0; i < 200; i++ {
		m = appendSystemLine(m, "common word line")
	}
	m = typeLine(m, "/search common")
	body := systemText(m)
	if !strings.Contains(body, "and") || !strings.Contains(body, "more") {
		t.Fatalf("a 200-hit search did not report that it was truncated: %q", tail(body, 300))
	}
	// The rendered hits are the rows beginning "  <window>". Counting the word "line" would count
	// the 200 stuffed source lines too, which is the mistake this test made first.
	if n := strings.Count(body, "\n  #"); n > 60 {
		t.Fatalf("%d rendered hits: the cap did not hold", n)
	}
}

// searchFixture builds a model with two windows, each holding a line, so a cross-window search has
// something to find in both.
func searchFixture(t *testing.T) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming{Window: "status", Nick: "srv", Text: "connected", Kind: LineSystem})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "the spec again", Kind: LineMessage})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "a needle in this channel", Kind: LineMessage})
	m, _ = m.Step(Incoming{Window: "#other", Nick: "carol", Text: "another needle over here", Kind: LineMessage})
	// Show a channel, so the search output lands somewhere visible.
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
	return m
}

// systemText is everything in the active window, joined.
func systemText(m Model) string {
	var b strings.Builder
	for _, l := range m.windows[m.active] {
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}

// appendSystemLine writes a system line into the active window through the model's own path, so a
// test that stuffs a window uses the same door the program does.
func appendSystemLine(m Model, text string) Model {
	m, _ = m.Step(Incoming{Window: m.active, Nick: "", Text: text, Kind: LineSystem})
	return m
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
