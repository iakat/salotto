package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// A nick is a link. Clicking one in the conversation or in the member list opens the query
// with that person, through the same door /msg uses. These tests click the frame rather than a
// geometry built here: a test that computes its own row and column cannot catch a renderer
// that moved the nick, and moving the nick is exactly what a timestamp, a style or a wrap does.

func clickAt(t *testing.T, m Model, x, y int) Model {
	t.Helper()
	next, _ := m.Step(tea.MouseMsg{
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
		X:      x,
		Y:      y,
	})
	return next
}

// displayColumn converts a byte offset in a drawn row to the cell column a mouse event uses.
// The frame is full of multi-byte box glyphs, so a byte index on its own points several
// columns to the right of the character the test found.
func displayColumn(row string, at int) int {
	return lipgloss.Width(row[:at])
}

// cellOf finds the first drawn cell containing needle and returns its column and row.
func cellOf(t *testing.T, m Model, needle string) (x, y int) {
	t.Helper()
	for row, line := range rowsOf(m) {
		if i := strings.Index(line, needle); i >= 0 {
			return displayColumn(line, i), row
		}
	}
	t.Fatalf("%q is not drawn anywhere:\n%s", needle, strings.Join(rowsOf(m), "\n"))
	return 0, 0
}

// memberCellOf finds a nick in the member list, which is the last column before the right
// border. The conversation often contains the same name, so a plain search of the whole row
// would click the body and this test would pass for the wrong reason.
func memberCellOf(t *testing.T, m Model, nick string) (x, y int) {
	t.Helper()
	for row, line := range rowsOf(m) {
		parts := strings.Split(line, "│")
		if len(parts) < 3 {
			continue
		}
		if strings.Contains(parts[len(parts)-2], nick) {
			return displayColumn(line, strings.LastIndex(line, nick)), row
		}
	}
	t.Fatalf("%q is not drawn in the member list:\n%s", nick, strings.Join(rowsOf(m), "\n"))
	return 0, 0
}

// TestClickingANickInTheBodyOpensAQuery is the reported interaction: a click on a name in the
// conversation opens the query with that person. Returning early from the click handler leaves
// the user in the channel, which is the mutation this test exists to catch.
func TestClickingANickInTheBodyOpensAQuery(t *testing.T) {
	m := sessionModel(t, 120, 24)
	x, y := cellOf(t, m, "<alice>")

	next := clickAt(t, m, x+1, y) // the 'a', not the '<'
	if got := winName(next.Active()); got != "alice" {
		t.Fatalf("clicking alice in the body left the active window at %q", got)
	}
	if _, ok := next.windows[next.keyFor("alice")]; !ok {
		t.Fatalf("the query window does not exist; windows are %v", next.order)
	}
}

// TestClickingTheTimestampDoesNotOpenAQuery pins the left edge of the span: the time is drawn
// before the nick but is not part of it. A hit test whose span starts at column zero turns the
// clock into a link.
func TestClickingTheTimestampDoesNotOpenAQuery(t *testing.T) {
	m := sessionModel(t, 120, 24)
	when := time.Date(2026, 11, 25, 12, 34, 56, 0, time.Local)
	m, _ = m.Step(Incoming(Line{
		Window: "#salotto", Network: "libera", Nick: "alice",
		Text: "a timed line", Kind: LineMessage, Time: when,
	}))

	stamp := when.Format("15:04:05")
	x, y := cellOf(t, m, stamp+" <alice>")
	// Every column of the time is a miss, not just the first: a span that starts at column
	// zero and one that forgets the timestamp's width are both wrong in the same way.
	for offset := 0; offset < len(stamp); offset++ {
		next := clickAt(t, m, x+offset, y)
		if got := winName(next.Active()); got != "#salotto" {
			t.Fatalf("a click on timestamp column %d opened the query for %q", offset, got)
		}
		if _, ok := next.windows[next.keyFor("alice")]; ok {
			t.Fatalf("a click on timestamp column %d created an alice window", offset)
		}
	}
}

// TestClickingAContinuationRowDoesNotOpenAQuery: only the first row of a wrapped message
// carries the nick. Treating every row as clickable makes a click on a word inside a paragraph
// open a query, which is the mutation this test pins.
func TestClickingAContinuationRowDoesNotOpenAQuery(t *testing.T) {
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 40, Height: 14})
	m, _ = m.Step(Incoming(Line{
		Window: "#test", Nick: "alice",
		Text: strings.Repeat("a long sentence wraps several times ", 4),
		Kind: LineMessage,
	}))

	_, y := cellOf(t, m, "<alice>")
	if y+1 >= len(rowsOf(m)) {
		t.Fatal("setup: the message did not wrap to a second row")
	}
	// The nick's column on the continuation row is the indent under the body, not a link.
	next := clickAt(t, m, 2, y+1)
	if got := winName(next.Active()); got != "#test" {
		t.Fatalf("a click on the wrapped message's second row opened %q", got)
	}
	if _, ok := next.windows[next.keyFor("alice")]; ok {
		t.Fatal("a click on a continuation row created an alice window")
	}
}

// TestClickingAJoinLineDoesNotOpenAQuery: a join names a person but is not a conversation with
// them. Falling back to the line's nick for every kind makes an event row a link.
func TestClickingAJoinLineDoesNotOpenAQuery(t *testing.T) {
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 20})
	m, _ = m.Step(Incoming(Line{
		Window: "#test", Nick: "alice", Text: "has joined", Kind: LineJoin,
	}))

	x, y := cellOf(t, m, "alice")
	next := clickAt(t, m, x, y)
	if got := winName(next.Active()); got != "#test" {
		t.Fatalf("a click on a join line opened %q", got)
	}
	if _, ok := next.windows[next.keyFor("alice")]; ok {
		t.Fatal("a click on a join line created an alice window")
	}
}

// TestClickingOurOwnNickDoesNotOpenAQuery: a query addressed to ourselves can only ever talk
// to us, and a misclick leaves a window the user has to close. Skipping the self check makes
// this test fail.
func TestClickingOurOwnNickDoesNotOpenAQuery(t *testing.T) {
	m := sessionModel(t, 120, 24)
	m, _ = m.Step(Incoming(Line{
		Window: "#salotto", Network: "libera", Nick: "nicholas",
		Text: "a line of my own", Kind: LineMessage, Own: true,
	}))

	x, y := cellOf(t, m, "<nicholas>")
	next := clickAt(t, m, x+1, y)
	if got := winName(next.Active()); got != "#salotto" {
		t.Fatalf("clicking our own nick opened %q", got)
	}
	if _, ok := next.windows[next.keyFor("nicholas")]; ok {
		t.Fatal("clicking our own nick created a query window")
	}
}

// TestClickingANickInTheMemberListOpensAQuery: the member list is the other obvious place a
// name is a person. Handling only the body leaves it a dead zone, which is the mutation here.
func TestClickingANickInTheMemberListOpensAQuery(t *testing.T) {
	m := sessionModel(t, 120, 24)
	x, y := memberCellOf(t, m, "alice")

	// The prefix belongs to the label, not to the name: clicking it must not open a query.
	prefixMiss := clickAt(t, m, x-1, y) // the '@'
	if got := winName(prefixMiss.Active()); got != "#salotto" {
		t.Fatalf("a click on the member prefix opened %q", got)
	}

	next := clickAt(t, m, x, y)
	if got := winName(next.Active()); got != "alice" {
		t.Fatalf("clicking alice in the member list left the active window at %q", got)
	}
}

// TestTheIndexAgreesWithTheRenderedRows is the assertion that keeps the two outputs of the one
// loop honest. A renderer that counts a wrapped row while the index forgets it shifts every
// click below that line; comparing lengths at several widths is what catches it, and a row
// that draws text must point at a line rather than at the padding the two outputs share.
func TestTheIndexAgreesWithTheRenderedRows(t *testing.T) {
	m := sessionModel(t, 120, 24)
	for _, width := range []int{24, 40, 120} {
		lines := m.bodyLines(width, 200)
		index := m.bodyIndex(width, 200)
		if len(index) != len(lines) {
			t.Fatalf("at width %d the body has %d rows and the index has %d", width, len(lines), len(index))
		}
		for i, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			if index[i].line < 0 {
				t.Fatalf("at width %d row %d draws %q but the index has no line behind it",
					width, i, line)
			}
		}
	}
}

// TestClickingAnExistingQuerySwitchesToIt: a click is a way to get to a conversation, not a way
// to start a new one. Clearing on open throws away the history the window already holds; this
// test fails if the click path creates a second window or empties the first.
func TestClickingAnExistingQuerySwitchesToIt(t *testing.T) {
	m := sessionModel(t, 120, 24)
	m, _ = m.Step(Incoming(Line{
		Window: "alice", Nick: "alice", Text: "an earlier conversation", Kind: LineMessage,
	}))
	m = m.SetActive(m.keyFor("#salotto"))
	key := m.keyFor("alice")
	history := len(m.windows[key])
	windows := len(m.order)
	if history == 0 {
		t.Fatal("setup: the query has no history to keep")
	}

	x, y := cellOf(t, m, "<alice>")
	next := clickAt(t, m, x+1, y)
	if got := winName(next.Active()); got != "alice" {
		t.Fatalf("the click landed in %q, not in the existing query", got)
	}
	if got := len(next.windows[key]); got != history {
		t.Fatalf("the existing query holds %d lines after the click, want %d", got, history)
	}
	if got := len(next.order); got != windows {
		t.Fatalf("the click changed the window count from %d to %d", windows, got)
	}
}
