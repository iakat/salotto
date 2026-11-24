package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestGoSwitchesWindowsByFuzzyPattern is "/go for switching channels with fuzzy matching".
//
// The command exists to avoid typing a channel name, so an exact match would defeat it: the
// searches a user actually types are prefixes ("/go sal") and abbreviations ("/go st"),
// and both have to land. It is resolved in the model because the window list is the model's
// - a bouncer does not know which windows this client is showing, so there is nothing it
// could do with the command if it were sent.
func TestGoSwitchesWindowsByFuzzyPattern(t *testing.T) {
	m := windowModelFor("status", "#salotto", "#salotto-probe", "#test")

	if got := typeLine(m, "/go sal").Active(); got != "#salotto" {
		t.Fatalf("/go sal landed on %q", got)
	}
	// "#salotto" and "#salotto-probe" both match "sal"; the shorter name is the better
	// match, which is what someone typing a prefix means.
	if got := typeLine(m, "/go salotto-p").Active(); got != "#salotto-probe" {
		t.Fatalf("/go salotto-p landed on %q", got)
	}
	if got := typeLine(m, "/go st").Active(); got != "status" {
		t.Fatalf("/go st landed on %q", got)
	}
	if got := typeLine(m, "/go #test").Active(); got != "#test" {
		t.Fatalf("/go #test landed on %q", got)
	}
}

// TestGoNeverLeavesTheClient is the wire property that makes this safe.
//
// A /go that also reached the server would be answered with "421 Unknown command", so a
// working switch and a correct search would look like a broken session. It has to be
// swallowed.
func TestGoNeverLeavesTheClient(t *testing.T) {
	m := windowModelFor("status", "#salotto")
	sent := typeLineCmd(m, "/go sal")
	if sent != nil {
		t.Fatalf("/go produced a command for the transport: %v", sent())
	}
}

// TestGoWithNoMatchSaysSo: silence looks like a typo in the window name, a dead key, or a
// frozen client, and the user has no way to tell which.
func TestGoWithNoMatchSaysSo(t *testing.T) {
	m := windowModelFor("status", "#salotto")
	after := typeLine(m, "/go zzzz")
	if after.Active() != "status" {
		t.Fatalf("/go with no match moved to %q", after.Active())
	}
	lines := after.windows["status"]
	if len(lines) == 0 || !strings.Contains(lines[len(lines)-1].Text, "zzzz") {
		t.Fatalf("/go with no match said nothing: %v", lines)
	}
}

// TestGoPrefersAnExactMatch is the tie-break. Fuzzy scoring gives a short name and a long
// name the same score for a prefix pattern, so without a bonus the winner is whichever
// happens to come first in the window list - and the user typed the window's whole name.
func TestGoPrefersAnExactMatch(t *testing.T) {
	// "sal" first, so an unbonused tie would pick it.
	m := windowModelFor("#salotto", "#sal")

	if got := typeLine(m, "/go sal").Active(); got != "#sal" {
		t.Fatalf("/go sal landed on %q, want the exact match", got)
	}
}

// TestGoAloneDoesNothing: a user who has typed "/go" and not pressed anything else has not
// asked to go anywhere. Jumping to an arbitrary window would be a surprise they cannot undo
// by typing the rest.
func TestGoAloneDoesNothing(t *testing.T) {
	m := windowModelFor("status", "#salotto")
	if got := typeLine(m, "/go").Active(); got != "status" {
		t.Fatalf("/go alone moved to %q", got)
	}
}

// TestTheWheelScrollsTheConversation is "also add scrolling".
//
// The program runs on the alt screen, where there is no terminal scrollback to fall back on,
// so a wheel notch that the model ignores does nothing at all - the wheel appears broken
// rather than unsupported. Wheel events arrive as mouse messages with coordinates, so a
// press handler that does not check the button switches window every time the user scrolls
// over the sidebar.
func TestTheWheelScrollsTheConversation(t *testing.T) {
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 24})
	for i := 0; i < 40; i++ {
		m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "line", Kind: LineMessage}))
	}
	before := m.scroll

	m, _ = m.Step(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp, X: 30, Y: 10})
	if m.scroll <= before {
		t.Fatalf("the wheel did not scroll: %d -> %d", before, m.scroll)
	}
	up := m.scroll
	m, _ = m.Step(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown, X: 30, Y: 10})
	if m.scroll >= up {
		t.Fatalf("the wheel did not scroll back: %d -> %d", up, m.scroll)
	}
}

// TestTheWheelNeverLeavesTheBuffer: scrolling up past the first line and down past the last
// is easy to do with a wheel, and an unclamped scroll is a blank window with no way out.
func TestTheWheelNeverLeavesTheBuffer(t *testing.T) {
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 24})
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "only", Kind: LineMessage}))

	for i := 0; i < 20; i++ {
		m, _ = m.Step(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	}
	if m.scroll > len(m.windows["#test"]) {
		t.Fatalf("scrolled %d past the first line of a %d-line buffer", m.scroll, len(m.windows["#test"]))
	}
	for i := 0; i < 40; i++ {
		m, _ = m.Step(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	}
	if m.scroll != 0 {
		t.Fatalf("scrolled down to %d, want 0", m.scroll)
	}
}

// TestTheWheelOverTheSidebarDoesNotSwitchWindows is the interaction between the two mouse
// features: a wheel notch carries X/Y, so a press handler that only looks at coordinates
// selects whatever window happens to be under the pointer when the user scrolls.
//
// The wheel has to land on a row that is NOT the active window, and the check has to happen
// before anything else moves the active window: an earlier click in the same test would make
// the assertion true no matter what the wheel did.
func TestTheWheelOverTheSidebarDoesNotSwitchWindows(t *testing.T) {
	m := windowModelFor("status", "#salotto", "#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 24})

	// Find the row that draws "#test", so the test is not coupled to the exact layout.
	y := -1
	for row := 0; row < 10 && y < 0; row++ {
		if win := m.WindowAt(3, row); win == "#test" {
			y = row
		}
	}
	if y < 0 {
		t.Fatal("setup: #test is not drawn in the sidebar")
	}

	// The control: on a copy of the same model, a left click at that cell really does
	// switch, so the assertion below is about the button and not about the coordinates.
	clicked, _ := m.Step(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, X: 3, Y: y})
	if clicked.Active() != "#test" {
		t.Fatalf("setup: a click on row %d selected %q, so this test proves nothing", y, clicked.Active())
	}

	// Every press that is not a left click. A vertical wheel returns before this point, so
	// the cases that reach it are the ones nobody thinks about: a tilting wheel (left and
	// right are wheel buttons too, not clicks) and a right click on the sidebar, which on a
	// phone is a long press.
	for _, b := range []tea.MouseButton{
		tea.MouseButtonWheelUp, tea.MouseButtonWheelDown,
		tea.MouseButtonWheelLeft, tea.MouseButtonWheelRight,
		tea.MouseButtonRight, tea.MouseButtonMiddle,
	} {
		got, _ := m.Step(tea.MouseMsg{Action: tea.MouseActionPress, Button: b, X: 3, Y: y})
		if got.active != "status" {
			t.Fatalf("a press with %v over the sidebar switched to %q", b, got.active)
		}
	}
}

// windowModelFor builds a model with the given windows in order, so a /go test has a list
// to search without going through a transport.
func windowModelFor(names ...string) Model {
	m := New(names[0])
	for _, n := range names[1:] {
		m.windows[n] = nil
		m.order = append(m.order, n)
	}
	return m
}

// typeLine types a line into the model and presses enter, returning the model as the
// keystrokes left it.
func typeLine(m Model, text string) Model {
	next, _ := typeLineStep(m, text)
	return next
}

// typeLineCmd is the same, returning the command the enter produced so a test can see
// whether anything was meant for the wire.
func typeLineCmd(m Model, text string) tea.Cmd {
	_, cmd := typeLineStep(m, text)
	return cmd
}

func typeLineStep(m Model, text string) (Model, tea.Cmd) {
	for _, ru := range text {
		if ru == ' ' {
			m, _ = m.Step(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ru}})
	}
	return m.Step(tea.KeyMsg{Type: tea.KeyEnter})
}
