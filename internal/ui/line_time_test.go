package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TestLinesCarryTimestamps is what makes a replayed buffer readable.
//
// A bouncer replays what happened while you were away. Without times on the lines, an
// overnight conversation is a wall of text whose only ordering clue is the order it arrived
// in - and "arrived" is the one thing a replay does not preserve.
func TestLinesCarryTimestamps(t *testing.T) {
	Configure()
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	at := time.Date(2026, 11, 24, 9, 41, 7, 0, time.Local)
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "morning", Kind: LineMessage, Time: at}))

	screen := plainText(m.View())
	if !strings.Contains(screen, "09:41:07") {
		t.Fatalf("the line has no HH:MM:SS timestamp:\n%s", screen)
	}
}

// TestTheTimestampIsTheServersNotTheClients is the replay case, stated as a test.
//
// A line that happened last night must show last night's time when it is replayed this
// morning. Stamping it on arrival would put a whole night's conversation in the minute the
// client reconnected, which is worse than no timestamp: it is confidently wrong.
func TestTheTimestampIsTheServersNotTheClients(t *testing.T) {
	Configure()
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	old := time.Date(2025, 1, 2, 3, 4, 5, 0, time.Local)
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "bob", Text: "last year", Kind: LineMessage, Time: old}))

	if screen := plainText(m.View()); !strings.Contains(screen, "03:04:05") {
		t.Fatalf("a replayed line lost its own time:\n%s", screen)
	}
}

// TestAWrappedLineKeepsItsTimestampColumn checks the arithmetic rather than the look. The
// wrap width has to be reduced by the timestamp before wrapping, or every long message in a
// busy channel spills one line past the frame - the border is the thing that breaks first,
// and a broken border is how this was found the last time.
func TestAWrappedLineKeepsItsTimestampColumn(t *testing.T) {
	Configure()
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 60, Height: 20})

	long := strings.Repeat("word ", 40)
	m, _ = m.Step(Incoming(Line{
		Window: "#test", Nick: "alice", Text: long,
		Kind: LineMessage, Time: time.Date(2026, 11, 24, 12, 0, 0, 0, time.Local),
	}))

	for _, line := range strings.Split(m.View(), "\n") {
		if w := lipgloss.Width(line); w > 60 {
			t.Fatalf("a line is %d columns wide in a 60-column frame: %q", w, plainText(line))
		}
	}
}

// TestANewDayGetsARule is the separator. It is inserted as a line so it scrolls with the
// conversation it belongs to; deriving it at draw time moves the boundary as you scroll.
func TestANewDayGetsARule(t *testing.T) {
	Configure()
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	before := time.Date(2026, 11, 23, 23, 58, 0, 0, time.Local)
	after := time.Date(2026, 11, 24, 0, 3, 0, 0, time.Local)
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "late", Kind: LineMessage, Time: before}))
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "later", Kind: LineMessage, Time: after}))

	days := 0
	for _, l := range m.windows["#test"] {
		if l.Kind == LineDay {
			days++
		}
	}
	if days != 1 {
		t.Fatalf("%d day rules for one midnight, want 1", days)
	}
	if screen := plainText(m.View()); !strings.Contains(screen, "24 Nov 2026") {
		t.Fatalf("the day rule does not name the day:\n%s", screen)
	}
}

// TestTheSameDayGetsNoRule is the other half, and the one that goes wrong when the
// comparison is on hours or on UTC days: two lines an hour apart must not be separated, and
// two lines either side of local midnight must be.
func TestTheSameDayGetsNoRule(t *testing.T) {
	Configure()
	m := New("#test")
	base := time.Date(2026, 11, 24, 1, 0, 0, 0, time.Local)
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "a", Text: "one", Kind: LineMessage, Time: base}))
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "a", Text: "two", Kind: LineMessage, Time: base.Add(22 * time.Hour)}))

	for _, l := range m.windows["#test"] {
		if l.Kind == LineDay {
			t.Fatalf("a day rule appeared between two lines of the same day")
		}
	}
}

// TestATopicGetsItsOwnRow is the complaint in one assertion: the topic was in the title bar,
// sharing space with the window name and the modes, and a real topic lost the argument.
func TestATopicGetsItsOwnRow(t *testing.T) {
	Configure()
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Topic{Window: "#test", Text: "welcome to the test channel, please read the rules"})

	screen := plainText(m.View())
	rows := strings.Split(screen, "\n")
	if !strings.Contains(rows[1], "welcome to the test channel") {
		t.Fatalf("the topic is not on the row under the title:\n%s", screen)
	}
	// And the frame is still exactly the size of the terminal: a new row is a row taken
	// from the conversation, not one added to the frame.
	if len(rows) != 30 {
		t.Fatalf("the frame is %d rows, want 30", len(rows))
	}
}

// TestNoTopicMeansNoTopicRow: a channel with no topic must not reserve a row. An empty band
// between the title and the conversation reads as a rendering fault, and it costs the
// conversation a line for nothing.
func TestNoTopicMeansNoTopicRow(t *testing.T) {
	Configure()
	withTopic := New("#test")
	withTopic, _ = withTopic.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	withTopic, _ = withTopic.Step(Topic{Window: "#test", Text: "a topic"})

	without := New("#test")
	without, _ = without.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	// The conversation loses a row exactly when there is a topic row to pay for.
	rowsWith := strings.Split(plainText(withTopic.View()), "\n")
	rowsWithout := strings.Split(plainText(without.View()), "\n")
	if len(rowsWith) != len(rowsWithout) {
		t.Fatalf("the frame is %d rows with a topic and %d without; both are the same terminal",
			len(rowsWith), len(rowsWithout))
	}
	if strings.Contains(strings.Join(rowsWithout, "\n"), "a topic") {
		t.Fatal("a window with no topic drew one")
	}
	// And the topic really is above the conversation, not in the title.
	if !strings.Contains(rowsWith[1], "a topic") {
		t.Fatalf("the topic row is not row 1:\n%s", strings.Join(rowsWith[:3], "\n"))
	}
}

// TestARefusalIsNotJustAnotherStatusLine pins the difference between "the server said
// something" and "the server refused you".
//
// Both used to be LineSystem and drawn in the same grey. A 403 after sending to a channel
// therefore read as another log line, and the user's next question - "why did my message not
// send" - had its answer already on screen, scrolled past. The error range gets its own kind so
// it can be drawn as one; every other numeric stays a plain status line, because turning the
// MOTD red would spend the one colour that means "this failed".
func TestARefusalIsNotJustAnotherStatusLine(t *testing.T) {
	for _, tc := range []struct {
		line string
		want LineKind
	}{
		{":srv 403 alice #chan :Cannot send to channel", LineError},
		{":srv 401 alice nobody :No such nick", LineError},
		{":srv 473 alice #chan :Cannot join channel (+i)", LineError},
		{":srv 500 alice :Too many clients", LineError},
		{":srv 001 alice :Welcome", LineSystem},
		{":srv 372 alice :- a line of the motd", LineSystem},
		// 366 is end-of-NAMES: it is membership, so it produces no line at all and is
		// tested by the nicklist tests instead. This is a numeric that does fall
		// through to status.
		{":srv 375 alice :- server Message of the day -", LineSystem},
	} {
		t.Run(tc.line, func(t *testing.T) {
			tr := newTestTransport(t)
			tr.handle([]byte(tc.line))
			line, ok := nextIncoming(t, tr)
			if !ok {
				t.Fatal("nothing came out of the transport")
			}
			if line.Window != "status" {
				t.Fatalf("window = %q, want status", line.Window)
			}
			if line.Kind != tc.want {
				t.Fatalf("kind = %v, want %v", line.Kind, tc.want)
			}
		})
	}
}

// TestAnErrorIsDrawnInItsOwnColour asserts on the frame, not on the kind: the kind is the
// plumbing, and what the user sees is the escape code. A test that stops at the kind would
// pass with every error drawing grey.
func TestAnErrorIsDrawnInItsOwnColour(t *testing.T) {
	l := Line{Window: "status", Text: "403 #chan :Cannot send to channel", Kind: LineError}
	got := strings.Join(lineLinesStyled(l, 60, DefaultSettings()), "\n")
	if !strings.Contains(got, "38;5;203") {
		t.Fatalf("an error is not drawn in the error colour: %q", got)
	}
	plain := Line{Window: "status", Text: "372 :- motd", Kind: LineSystem}
	if s := strings.Join(lineLinesStyled(plain, 60, DefaultSettings()), "\n"); strings.Contains(s, "38;5;203") {
		t.Fatalf("a plain status line was drawn as an error: %q", s)
	}
}
