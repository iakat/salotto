package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Search used to end at a numbered list of text written into the active window: an answer to "did I
// spell it right" that left the user to find the line themselves, in another window, possibly a
// thousand lines back. QUEUE item 3 is that gap - "cannot jump to result context" - and the fix is
// a result list you can walk: /search opens it, the arrows move, enter jumps to the hit with its
// context around it.

// TestSearchCommandFindsAcrossWindows: the command reaches the search, and the result list carries
// the window each hit came from - a hit with no window is a hit the user cannot act on.
func TestSearchCommandFindsAcrossWindows(t *testing.T) {
	m := searchFixture(t)

	m = typeLine(m, "/search spec")
	if len(m.searchHits) == 0 {
		t.Fatal("/search opened no result list")
	}
	hit := m.searchHits[0]
	if hit.Window != "#salotto" {
		t.Fatalf("the hit names window %q, want #salotto", hit.Window)
	}
	if !strings.Contains(hit.Line.Text, "spec again") {
		t.Fatalf("the hit carries %q", hit.Line.Text)
	}
}

// TestTheResultListIsDrawnInsteadOfTheConversation pins that the list is visible: a search that only
// changes state is a search the user cannot see.
func TestTheResultListIsDrawnInsteadOfTheConversation(t *testing.T) {
	m := searchFixture(t)
	m = typeLine(m, "/search needle")

	body := strings.Join(m.bodyLines(60, 12), "\n")
	if !strings.Contains(body, "matches") {
		t.Fatalf("the result list has no header: %q", body)
	}
	if !strings.Contains(body, "#salotto") || !strings.Contains(body, "#other") {
		t.Fatalf("the list does not place the hits: %q", body)
	}
	// The conversation is not drawn underneath: the list is a mode, and showing both would put
	// the hits and the buffer in one column with no way to tell them apart.
	if strings.Contains(body, "connected") {
		t.Fatalf("the conversation is still drawn behind the result list: %q", body)
	}
}

// TestSearchCrossesEveryWindow pins the cross-window half: a hit in a window the user is not looking
// at is found and can be reached.
func TestSearchCrossesEveryWindow(t *testing.T) {
	m := searchFixture(t)
	m = typeLine(m, "/search needle")

	seen := map[string]bool{}
	for _, hit := range m.searchHits {
		seen[hit.Window] = true
	}
	if !seen["#salotto"] || !seen["#other"] {
		t.Fatalf("hits found in %v, want both #salotto and #other", seen)
	}
}

// TestWalkingTheListJumpsToTheHit is the item: the arrows move the selection, and the window it
// lands on becomes active with the hit on screen.
func TestWalkingTheListJumpsToTheHit(t *testing.T) {
	m := searchFixture(t)
	m = typeLine(m, "/search needle")
	if len(m.searchHits) < 2 {
		t.Fatalf("the fixture produced %d hits, want at least 2", len(m.searchHits))
	}

	// The list starts on the first hit, which is in #salotto.
	if m.active != m.keyFor("#salotto") {
		t.Fatalf("before walking, the view is on %q", winName(m.active))
	}

	// One step down lands on the second hit, in #other.
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyDown})
	if winName(m.active) != "#other" {
		t.Fatalf("after one step the view is on %q, want #other", winName(m.active))
	}
	if m.searchAt != 1 {
		t.Fatalf("the selection is on hit %d, want 1", m.searchAt)
	}
}

// TestJumpingPutsTheHitOnScreen: arriving at the right window but at the bottom of it is a jump in
// name only. The hit has to be inside the viewport, with some context above it.
func TestJumpingPutsTheHitOnScreen(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 20})
	// A long buffer with the needle buried near the top, so the default view (the bottom) would
	// not show it.
	m, _ = m.Step(Incoming{Window: "#long", Nick: "bob", Text: "the needle is here", Kind: LineMessage})
	for i := 0; i < 200; i++ {
		m = appendSystemLineTo(m, "#long", "filler line")
	}
	m = typeLine(m, "/search needle")
	if len(m.searchHits) != 1 {
		t.Fatalf("%d hits, want 1", len(m.searchHits))
	}
	// Enter commits the jump and closes the list. Checking the body while the list is still up
	// would pass on the list's own text and never exercise the scroll - which is exactly how the
	// first version of this test survived a mutation that removed it.
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.searchHits) != 0 {
		t.Fatal("enter did not close the list")
	}
	if winName(m.active) != "#long" {
		t.Fatalf("the jump landed on %q, want #long", winName(m.active))
	}

	body := strings.Join(m.bodyLines(60, 12), "\n")
	if !strings.Contains(body, "needle") {
		t.Fatalf("the hit is not on screen after the jump; the view is:\n%s", body)
	}
	// And the buffer did not simply scroll to the top either: it is a jump to a line, and the
	// filler around it is what makes it read as context rather than as an empty window.
	if !strings.Contains(body, "filler") {
		t.Fatalf("the hit is on screen with no context around it; the view is:\n%s", body)
	}
}

// TestEscapeLeavesTheListWithoutMoving: cancelling a search must not undo the walk. The user found
// what they were looking for, and dragging them back to where they started would hide it again.
func TestEscapeLeavesTheListWithoutMoving(t *testing.T) {
	m := searchFixture(t)
	m = typeLine(m, "/search needle")
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyDown})
	landed, scroll := m.active, m.scroll

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEsc})
	if len(m.searchHits) != 0 {
		t.Fatal("escape did not close the list")
	}
	if m.active != landed {
		t.Fatalf("escape moved the view from %q to %q", winName(landed), winName(m.active))
	}
	if m.scroll != scroll {
		t.Fatalf("escape reset the scroll from %d to %d, hiding the line the user found",
			scroll, m.scroll)
	}
}

// TestTypingWhileTheListIsUpClosesItAndKeepsTheKeys: a mode that swallows typing traps the user.
func TestTypingWhileTheListIsUpClosesItAndKeepsTheKeys(t *testing.T) {
	m := searchFixture(t)
	m = typeLine(m, "/search needle")
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("h")})
	if len(m.searchHits) != 0 {
		t.Fatal("typing left the list up")
	}
	if string(m.input) != "h" {
		t.Fatalf("the keystroke went to the list instead of the input: %q", string(m.input))
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
	if len(m.searchHits) != 0 {
		t.Fatalf("an empty search opened %d hits", len(m.searchHits))
	}
}

// TestSearchIsNotEmpty pins "no answer" as a bug: /search with no argument is a usage message, not a
// search for the empty string, which would match every line ever shown.
func TestSearchIsNotEmpty(t *testing.T) {
	m := searchFixture(t)

	m = typeLine(m, "/search")
	body := systemText(m)
	if !strings.Contains(body, "usage") {
		t.Fatalf("/search alone said %q", body)
	}
	if len(m.searchHits) != 0 {
		t.Fatalf("an empty /search ran a search: %d hits", len(m.searchHits))
	}
}

// TestSearchDoesNotGoToTheWire pins that the command is resolved in the model. The scrollback is
// the client's; sending SEARCH upstream would return the bouncer's ring instead of what this client
// has shown, which is a different and smaller thing.
func TestSearchDoesNotGoToTheWire(t *testing.T) {
	if cmd := typeLineCmd(searchFixture(t), "/search salotto"); cmd != nil {
		t.Fatal("the search produced a command for the wire; it must be resolved in the model")
	}
}

// TestALongResultListKeepsTheSelectionVisible pins the scrolling of the list itself: walking past
// the bottom of a long result set and watching the highlight leave the screen is how a picker
// becomes unusable exactly when the search was worth running.
func TestALongResultListKeepsTheSelectionVisible(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 20})
	for i := 0; i < 100; i++ {
		m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "common word", Kind: LineMessage})
	}
	m = typeLine(m, "/search common")
	if len(m.searchHits) < 100 {
		t.Fatalf("%d hits, want at least 100", len(m.searchHits))
	}

	// Walk well past a screenful.
	for i := 0; i < 40; i++ {
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyDown})
	}
	body := strings.Join(m.bodyLines(60, 12), "\n")
	// The header reports the position...
	if !strings.Contains(body, "41 of") {
		t.Fatalf("the list lost track of the selection; body:\n%s", body)
	}
	// ...and the selected row is among the rows actually drawn, not scrolled off. Walking past a
	// screenful and seeing the highlight leave the screen is the failure this guards.
	if !strings.Contains(stripANSI(body), "> #salotto") {
		t.Fatalf("the selected row is not among the rendered rows; body:\n%s", body)
	}
	// The selected row is marked, and the marker is the row prefix. Testing for a bare ">" is
	// what let a mutation that removed the marker survive: every row carries one, in "<nick>".
	// The marker is wrapped in the reverse-video escape, so the row does not literally begin
	// with ">": strip the styling before looking for the prefix.
	marked := 0
	for _, ln := range strings.Split(body, "\n") {
		if strings.HasPrefix(stripANSI(ln), "> ") {
			marked++
		}
	}
	if marked != 1 {
		t.Fatalf("%d rows marked as selected, want exactly 1; body:\n%s", marked, body)
	}
}

// TestAHitIsPlacedByItsWholeIdentity.
//
// Two lines can read exactly the same - the same words from two people - and they are not the same
// line. Finding the hit by text alone lands the view on whichever one is nearer the bottom, which is
// a jump to somewhere the user was not looking.
func TestAHitIsPlacedByItsWholeIdentity(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = m.Step(Incoming{Window: "#dup", Nick: "alice", Text: "same words", Kind: LineMessage})
	for i := 0; i < 30; i++ {
		m = appendSystemLineTo(m, "#dup", "filler")
	}
	m, _ = m.Step(Incoming{Window: "#dup", Nick: "bob", Text: "same words", Kind: LineMessage})

	m = typeLine(m, "/search same words")
	if len(m.searchHits) != 2 {
		t.Fatalf("%d hits, want 2", len(m.searchHits))
	}
	// The first hit is alice's, which is the one further from the bottom.
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})

	anthology := m.windows[m.active]
	if m.searchJumpedAt < 0 || m.searchJumpedAt >= len(anthology) {
		t.Fatalf("the jump recorded position %d in a %d-line window", m.searchJumpedAt, len(anthology))
	}
	if got := anthology[m.searchJumpedAt].Nick; got != "alice" {
		t.Fatalf("the jump landed on %q's line, want alice's", got)
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

// stripANSI removes the styling escape sequences a rendered row carries, so a test can look at
// what the user reads rather than at the bytes that colour it.
func stripANSI(s string) string {
	var b strings.Builder
	inEscape := false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEscape = true
		case inEscape && r == 'm':
			inEscape = false
		case !inEscape:
			b.WriteRune(r)
		}
	}
	return b.String()
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

// appendSystemLineTo writes a system line into a named window.
func appendSystemLineTo(m Model, window, text string) Model {
	m, _ = m.Step(Incoming{Window: window, Nick: "", Text: text, Kind: LineSystem})
	return m
}

// appendSystemLine writes a system line into the active window through the model's own path, so a
// test that stuffs a window uses the same door the program does.
func appendSystemLine(m Model, text string) Model {
	return appendSystemLineTo(m, m.active, text)
}
