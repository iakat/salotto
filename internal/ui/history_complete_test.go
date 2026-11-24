package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// historyFixture is a model with a few lines sent, so the up arrow has something to find.
func historyFixture(t *testing.T) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming{Window: "status", Nick: "srv", Text: "connected", Kind: LineSystem})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "hello", Kind: LineMessage})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
	for _, s := range []string{"first message", "second message", "third message"} {
		m = typeLine(m, s)
	}
	return m
}

func pressKey(m Model, t tea.KeyType) Model {
	next, _ := m.Step(tea.KeyMsg{Type: t})
	return next
}

// TestUpRecallsTheLastLineSent is the whole point of the feature: up arrow, last thing you said.
func TestUpRecallsTheLastLineSent(t *testing.T) {
	m := historyFixture(t)
	m = pressKey(m, tea.KeyUp)
	if got := string(m.input); got != "third message" {
		t.Fatalf("up arrow gave %q, want the last line sent %q", got, "third message")
	}
}

// TestUpWalksBackwardsThroughTheHistory - each press is one older line, and it stops at the oldest
// rather than wrapping to the newest (wrapping loses the sense of a position).
func TestUpWalksBackwards(t *testing.T) {
	m := historyFixture(t)
	want := []string{"third message", "second message", "first message", "first message"}
	for i, w := range want {
		m = pressKey(m, tea.KeyUp)
		if got := string(m.input); got != w {
			t.Fatalf("press %d: got %q, want %q", i+1, got, w)
		}
	}
}

// TestDownRestoresTheHalfTypedDraft - the reason this is not a slice index. Type half a message,
// look something up, come back: the half-message must still be there.
func TestDownRestoresTheHalfTypedDraft(t *testing.T) {
	m := historyFixture(t)
	m = typeTyped(m, "half a thought")
	m = pressKey(m, tea.KeyUp)
	if string(m.input) != "third message" {
		t.Fatalf("up should replace the draft, got %q", m.input)
	}
	m = pressKey(m, tea.KeyDown)
	if got := string(m.input); got != "half a thought" {
		t.Fatalf("down gave %q, want the draft back, %q", got, "half a thought")
	}
}

// TestUpWithNoHistoryDoesNothing - a fresh session must not put junk in the box.
func TestUpWithNoHistoryDoesNothing(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = pressKey(m, tea.KeyUp)
	if string(m.input) != "" {
		t.Fatalf("up with no history produced %q", m.input)
	}
}

// TestARepeatedLineIsStoredOnce - up doing nothing is better than up producing a duplicate.
func TestARepeatedLineIsStoredOnce(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = typeLine(m, "same")
	m = typeLine(m, "same")
	if len(m.history) != 1 {
		t.Fatalf("history has %d entries, want 1: %q", len(m.history), m.history)
	}
}

// TestScrollingMovedOffTheArrowKeys - the arrow keys now belong to history, so PgUp/PgDn must do
// the scrolling they gave up. A feature that takes a key without providing the replacement is a
// regression dressed as progress.
func TestScrollingMovedOffTheArrowKeys(t *testing.T) {
	m := historyFixture(t)
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "hello"})
	for i := 0; i < 40; i++ {
		m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "line", Kind: LineMessage})
	}
	before := m.scroll
	m = pressKey(m, tea.KeyPgUp)
	if m.scroll <= before {
		t.Fatalf("PgUp did not scroll: %d -> %d", before, m.scroll)
	}
	m = pressKey(m, tea.KeyPgDown)
	if m.scroll != before {
		t.Fatalf("PgDown did not restore: %d, want %d", m.scroll, before)
	}
}

// TestTabCompletesACommand - "/go" from "/g".
func TestTabCompletesACommand(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = typeTyped(m, "/jo")
	m = pressKey(m, tea.KeyTab)
	if got := string(m.input); got != "/join" {
		t.Fatalf("Tab gave %q, want /join", got)
	}
}

// TestTabCyclesThroughCandidates - a second Tab with the same prefix moves to the next one.
func TestTabCyclesThroughCandidates(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = typeTyped(m, "/")
	first := ""
	m = pressKey(m, tea.KeyTab)
	first = string(m.input)
	if first == "/" {
		t.Fatal("Tab completed nothing")
	}
	m = pressKey(m, tea.KeyTab)
	second := string(m.input)
	if second == first {
		t.Fatalf("second Tab did not advance: still %q", second)
	}
	// Cycling must terminate: pressing as many times as there are candidates under "/" returns to
	// the starting one. Derived from the model's own list, so adding a command cannot silently
	// break this test's arithmetic.
	// Two presses are already spent above (one to complete, one to advance), so n-1 more lands
	// back on the first candidate.
	n := len(m.commandCandidates("/"))
	for i := 0; i < n-1; i++ {
		m = pressKey(m, tea.KeyTab)
	}
	if string(m.input) != first {
		t.Fatalf("cycle did not come back round: %q, want %q", m.input, first)
	}
}

// TestTabCompletesAChannel - a word starting with # completes from the window list.
func TestTabCompletesAChannel(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "hi", Kind: LineMessage})
	m, _ = m.Step(Incoming{Window: "#salottissimo", Nick: "bob", Text: "hi", Kind: LineMessage})
	m = typeTyped(m, "/join #salott")
	m = pressKey(m, tea.KeyTab)
	got := string(m.input)
	if got != "/join #salotto" && got != "/join #salottissimo" {
		t.Fatalf("Tab gave %q, want a channel starting #salott", got)
	}
}

// TestTabCompletesANickInANickPosition - "/msg ali" should reach alice.
func TestTabCompletesANickInANickPosition(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "hi", Kind: LineMessage})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
	m, _ = m.Step(Nicklist{Window: "#salotto", Nicks: []Nick{{Name: "alice"}, {Name: "alina"}, {Name: "bob"}}})
	m = typeTyped(m, "/msg ali")
	m = pressKey(m, tea.KeyTab)
	got := string(m.input)
	if got != "/msg alice" && got != "/msg alina" {
		t.Fatalf("Tab gave %q, want a nick starting ali", got)
	}
}

// TestTabDoesNotCompleteTheMessageBody - completing inside a sentence would rewrite what the user
// is saying. "/msg alice hel" must not become a nick.
func TestTabDoesNotCompleteTheMessageBody(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "hi", Kind: LineMessage})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
	m, _ = m.Step(Nicklist{Window: "#salotto", Nicks: []Nick{{Name: "helper"}}})
	m = typeTyped(m, "/msg alice hel")
	before := string(m.input)
	m = pressKey(m, tea.KeyTab)
	if string(m.input) != before {
		t.Fatalf("Tab rewrote a message body: %q -> %q", before, m.input)
	}
}

// TestTabDoesNotCompleteOrdinaryWords - a sentence is not a nick lookup.
//
// The word has to be a *later* word in a plain sentence to test this: a bare first word returns
// before the nick logic is consulted at all, so a test that types just "hello" passes whether or
// not that logic is correct - which a mutation proved by surviving it. This types a sentence and
// puts the cursor on the last word, which is the case a user actually hits.
func TestTabDoesNotCompleteOrdinaryWords(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "hi", Kind: LineMessage})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
	m, _ = m.Step(Nicklist{Window: "#salotto", Nicks: []Nick{{Name: "hello_there"}}})
	m = typeTyped(m, "say hello")
	before := string(m.input)
	m = pressKey(m, tea.KeyTab)
	if string(m.input) != before {
		t.Fatalf("Tab rewrote a word in a sentence: %q -> %q", before, m.input)
	}
}

// TestTabWithNoCandidatesIsHarmless - a prefix that matches nothing must leave the line untouched.
func TestTabWithNoCandidatesIsHarmless(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = typeTyped(m, "/zzzz")
	before := string(m.input)
	m = pressKey(m, tea.KeyTab)
	if string(m.input) != before {
		t.Fatalf("Tab with no candidates changed %q to %q", before, m.input)
	}
}

// TestCompletionSendsNothing - Tab is a client-side edit. A completed command that had already been
// sent would be a message the user never chose to send.
func TestCompletionSendsNothing(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = typeTyped(m, "/jo")
	var cmd tea.Cmd
	m, cmd = m.Step(tea.KeyMsg{Type: tea.KeyTab})
	if cmd != nil {
		if _, isSent := cmd().(Sent); isSent {
			t.Fatal("Tab produced a Sent command")
		}
	}
	if !strings.HasPrefix(string(m.input), "/join") {
		t.Fatalf("input %q", m.input)
	}
}

// typeTyped types a line without submitting it, for key-handling tests.
func typeTyped(m Model, s string) Model {
	for _, ru := range s {
		if ru == ' ' {
			m, _ = m.Step(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ru}})
	}
	return m
}
