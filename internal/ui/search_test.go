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
	hits := 0
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(ln, "  ") && strings.Contains(ln, "  <") {
			hits++
		}
	}
	if hits > 60 {
		t.Fatalf("%d rendered hits: the cap did not hold", hits)
	}
	// And the cap is the terminal's height, so the count line is still on the screen with them.
	// A fixed cap is what this test is here to prevent: 50 hits in a 10-row window pushes the
	// count, which is the answer to the question, off the top.
	if got := m.height - 4; got >= 5 && hits > got {
		t.Fatalf("%d hits in a %d row terminal: the list outgrew its own window", hits, m.height)
	}
}

// TestTheSearchListFitsTheTerminal pins that a tall result in a short terminal keeps its count
// line visible. This was a live defect: with a fixed cap of 50, the count scrolled off the top of a
// 30-row screen and the user saw the tail of a list with no idea how long it was.
func TestTheSearchListFitsTheTerminal(t *testing.T) {
	for _, height := range []int{12, 20, 40} {
		m := New("status")
		m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: height})
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
		for i := 0; i < 200; i++ {
			m = appendSystemLine(m, "common word line")
		}
		m = typeLine(m, "/search common")
		body := systemText(m)
		if !strings.Contains(body, "match(es)") {
			t.Fatalf("height %d: no count line", height)
		}
		// A hit row is "  <window>  <nick> text". The window is whatever the line was in, which
		// is not always a channel - matching only "#" here is how this test first reported "no
		// hits at all" while the list was plainly on screen.
		hits := 0
		for _, ln := range strings.Split(body, "\n") {
			if strings.HasPrefix(ln, "  ") && strings.Contains(ln, "  <") {
				hits++
			}
		}
		if hits == 0 {
			t.Fatalf("height %d: no hits at all", height)
		}
		// The rendered answer has to fit the terminal, or its own count scrolls away.
		if rendered := hits + 1; rendered > height {
			t.Fatalf("height %d: the answer renders %d rows, more than the screen", height, rendered)
		}
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
