package ui

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The tests for buffer activity, /next and /prev and the chat monitor assert on the drawn
// frame, not on the struct that feeds it. A rank is invisible until it becomes a colour or a
// badge, and a badge is invisible until it is a cell on screen; a test that stopped at
// activityOf would pass on a sidebar that draws every row the same.

// activityModel is a connected client sitting on its status window with nothing unread. Every
// window a test needs is created by sending it a line through Step, the same door the
// transport uses, so the test cannot build a state a real client could not reach.
func activityModel(t *testing.T) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 120, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "connected", Kind: LineSystem}))
	return m
}

// say sends one message to a window that is not the active one, which is what makes it count
// as unread.
func say(m Model, window, nick, text string) Model {
	m, _ = m.Step(Incoming(Line{Window: window, Network: "libera", Nick: nick, Text: text, Kind: LineMessage}))
	return m
}

// sidebarCellFor returns the raw sidebar cell of the row naming a window, ANSI and all. The
// raw cell is deliberate: the colour is the thing under test, and rowsOf strips exactly it.
func sidebarCellFor(t *testing.T, m Model, name string) string {
	t.Helper()
	for _, row := range strings.Split(m.View(), "\n") {
		parts := strings.Split(row, "│")
		if len(parts) < 2 {
			continue
		}
		if strings.Contains(plainText(parts[1]), name) {
			return parts[1]
		}
	}
	t.Fatalf("no sidebar row names %q:\n%s", name, plainText(m.View()))
	return ""
}

// conversationText is the conversation column with ANSI removed, so a monitor assertion is
// about what the view says and not about a sidebar row that happens to name the same window.
func conversationText(m Model) string {
	l := m.layout()
	idx := 1
	if l.winWidth > 0 {
		idx = 2
	}
	var b strings.Builder
	for _, row := range strings.Split(m.View(), "\n") {
		parts := strings.Split(row, "│")
		if idx >= len(parts) {
			continue
		}
		b.WriteString(plainText(parts[idx]))
		b.WriteString("\n")
	}
	return b.String()
}

// TestAMentionOutranksPlainChatter is the ranking made visible. A window somebody addressed
// us in and a window that merely has traffic are two different ranks, and the drawn list has
// to say so: if both drew the same colour, /next! and /next would look like the same command.
//
// The mutation is the rank comparison turning > into >=, which drops every message window into
// the highlight branch: the plain-chatter row would then carry the warm highlight colour and
// this test goes red.
func TestAMentionOutranksPlainChatter(t *testing.T) {
	m := activityModel(t)
	m.settings.Highlights = []string{"salotto"}
	m = say(m, "#chat", "bob", "just some chatter")
	m = say(m, "#mentions", "bob", "salotto is the word")

	highlight := m.keyFor("#mentions")
	chatter := m.keyFor("#chat")
	if got := m.activityOf(highlight); got != ActivityHighlight {
		t.Fatalf("the window with our highlight word ranks %v, want ActivityHighlight", got)
	}
	if got := m.activityOf(chatter); got != ActivityMessage {
		t.Fatalf("the window with plain chatter ranks %v, want ActivityMessage", got)
	}

	highlighted := sidebarCellFor(t, m, "#mentions")
	plain := sidebarCellFor(t, m, "#chat")
	if !strings.Contains(highlighted, "38;5;222") {
		t.Fatalf("the highlighted row is not drawn in the highlight colour:\n%q", highlighted)
	}
	if strings.Contains(plain, "38;5;222") {
		t.Fatalf("plain chatter is drawn in the highlight colour, so a mention does not outrank it:\n%q", plain)
	}
	if !strings.Contains(plain, "38;5;117") {
		t.Fatalf("the chatter row is not drawn in the message colour:\n%q", plain)
	}
}

// TestNextSkipsABufferThatDidNotChange: /next is "take me to what changed", so a window with
// nothing new must not stop the walk. The mutation is to ignore activity and land on the next
// row in the list, which is exactly the buffer the user has already read.
func TestNextSkipsABufferThatDidNotChange(t *testing.T) {
	m := activityModel(t)
	m = say(m, "#a-news", "bob", "a")
	m = say(m, "#b-read", "bob", "b")
	m = say(m, "#c-news", "bob", "c")
	// Looking at #b is what marks it read; going back to #a leaves it read while #c is still
	// new.
	m = m.SetActive(m.keyFor("#b-read"))
	m = m.SetActive(m.keyFor("#a-news"))

	m = typeLine(m, "/next")
	if got := winName(m.active); got != "#c-news" {
		t.Fatalf("/next landed on %q, past the read buffer it should have skipped", got)
	}
}

// TestNextBangSkipsToTheNextHighlight: the exclamation form is "something for me", not
// "anything new". A buffer with ordinary chatter is exactly what it must pass over.
//
// The mutation is to treat a message as a highlight, at which point /next! stops on #b and the
// two commands differ in spelling only.
func TestNextBangSkipsToTheNextHighlight(t *testing.T) {
	m := activityModel(t)
	m.settings.Highlights = []string{"salotto"}
	m = say(m, "#a-chat", "bob", "a")
	m = say(m, "#b-chatter", "bob", "b")
	m = say(m, "#z-highlight", "bob", "salotto, over here")
	m = m.SetActive(m.keyFor("#a-chat"))

	m = typeLine(m, "/next!")
	if got := winName(m.active); got != "#z-highlight" {
		t.Fatalf("/next! landed on %q, not on the only highlighted window", got)
	}
}

// TestNavigationFollowsTheSidebarOrder: the sidebar sorts windows under their server, so
// arrival order and drawn order are different lists. /next must walk the drawn one - a jump
// that follows arrival order appears to skip rows the user can see between here and there.
//
// The mutation is to iterate m.order, which was opened as #zeta then #alpha and is therefore
// the reverse of what the sidebar draws.
func TestNavigationFollowsTheSidebarOrder(t *testing.T) {
	m := activityModel(t)
	m = say(m, "#zeta", "bob", "z")
	m = say(m, "#alpha", "bob", "a")

	drawn := m.navigationOrder()
	if len(drawn) < 3 || winName(drawn[1]) != "#alpha" || winName(drawn[2]) != "#zeta" {
		t.Fatalf("setup: the sidebar does not sort #alpha before #zeta: %v", drawn)
	}

	m = typeLine(m, "/next")
	if got := winName(m.active); got != "#alpha" {
		t.Fatalf("/next landed on %q; the sidebar draws #alpha first", got)
	}
}

// TestNavigationWraps: from a read window after the last changed one, /next reaches the first
// changed window instead of stopping at the end. Without the wrap the walk finds nothing to
// do and the user has to know they are at the bottom of a list they cannot see in full.
//
// The starting window is read on purpose: a changed window clears itself when it becomes
// active, so sitting on the last changed one would not test a wrap at all - a walk that stops
// at the end would still reach the changed window before it.
//
// The mutation is to stop when the walk reaches the end of the list.
func TestNavigationWraps(t *testing.T) {
	m := activityModel(t)
	m = say(m, "#a-news", "bob", "a")
	m = say(m, "#z-read", "bob", "z")
	m = m.SetActive(m.keyFor("#z-read"))

	m = typeLine(m, "/next")
	if got := winName(m.active); got != "#a-news" {
		t.Fatalf("/next from the last row landed on %q; the only changed window is behind it", got)
	}
}

// TestTheMonitorShowsLinesFromOtherWindows: the whole point of the view is a line from a
// window this client never opened. The mutation is to build the monitor from the active
// window only, which is the window the user can already read and therefore not a monitor.
func TestTheMonitorShowsLinesFromOtherWindows(t *testing.T) {
	m := activityModel(t)
	m.settings.Highlights = []string{"salotto"}
	m = say(m, "#hidden", "bob", "salotto, come and look")

	m = typeLine(m, "/monitor")
	body := conversationText(m)
	if !strings.Contains(body, "come and look") {
		t.Fatalf("the monitor does not show a line from a window that was never opened:\n%s", body)
	}
}

// TestTheMonitorNamesTheSourceWindow: a line drawn without its window is a quote with no
// attribution, and the reason to read across buffers is to know where each line came from.
//
// The mutation is to drop the label; the sidebar still names #hidden, so this assertion reads
// only the conversation column.
func TestTheMonitorNamesTheSourceWindow(t *testing.T) {
	m := activityModel(t)
	m.settings.Highlights = []string{"salotto"}
	m = say(m, "#hidden", "bob", "salotto, come and look")

	m = typeLine(m, "/monitor")
	body := conversationText(m)
	if !strings.Contains(body, "#hidden") {
		t.Fatalf("the monitor row does not name the window it came from:\n%s", body)
	}
}

// TestTheMonitorRefusesInput: a line typed in a view that crosses buffers has no channel to
// belong to, so it must be refused where the user is looking rather than sent to whichever
// window was active before the monitor opened.
//
// The mutation is to fall through to Sent, which puts the message on the wire and returns a
// command this test can see.
func TestTheMonitorRefusesInput(t *testing.T) {
	m := activityModel(t)
	m = typeLine(m, "/monitor")

	cmd := typeLineCmd(m, "hello")
	if cmd != nil {
		if sent, ok := cmd().(Sent); ok {
			t.Fatalf("typing in the monitor sent %q to the bouncer", sent.Text)
		}
	}
	body := conversationText(m)
	if !strings.Contains(body, "read-only") {
		t.Fatalf("the monitor did not say it refused the line:\n%s", body)
	}
}

// TestTheMonitorIsNotABuffer: the monitor is a view, so it has no number and no badge. A
// number would put it in the /b list, where a jump target that is not a conversation is a
// dead end.
//
// The mutation is to assign it a number, which prefixes the sidebar row and adds it to /b.
func TestTheMonitorIsNotABuffer(t *testing.T) {
	m := activityModel(t)
	m = typeLine(m, "/monitor")

	cell := strings.TrimSpace(plainText(sidebarCellFor(t, m, "monitor")))
	cell = strings.TrimSpace(strings.TrimPrefix(cell, "▸"))
	if cell != "monitor" {
		t.Fatalf("the monitor row reads %q; it must have no buffer number and no badge", cell)
	}
	if n := m.bufferNo[monitorKey]; n != 0 {
		t.Fatalf("the monitor was given buffer number %d", n)
	}
	if list := m.bufferList(); strings.Contains(list, "monitor") {
		t.Fatalf("the monitor appears in the /b list: %q", list)
	}
	if got := m.Unread()[monitorKey]; got != 0 {
		t.Fatalf("the monitor has an unread count of %d", got)
	}
}

// TestTheMonitorDoesNotCopyHistory is the architectural one. The monitor must be derived from
// the windows on demand, walking only a bounded tail of each; the rejected alternative was a
// monitor slice appended to on every incoming line, which doubles the memory of a
// long-running daemon's scrollback and leaves two copies to drift apart after a close.
//
// The mutation is that append: 600 highlighted lines would then make the monitor 600 lines
// long instead of at most monitorTail, and the oldest line would still be there.
func TestTheMonitorDoesNotCopyHistory(t *testing.T) {
	m := activityModel(t)
	m.settings.Highlights = []string{"salotto"}
	for i := 0; i < 100; i++ {
		m = say(m, "#flood", "bob", "salotto pre "+strconv.Itoa(i))
	}
	m = typeLine(m, "/monitor")
	for i := 0; i < 500; i++ {
		m = say(m, "#flood", "bob", "salotto post "+strconv.Itoa(i))
	}

	if got := len(m.monitorMessages()); got > monitorTail {
		t.Fatalf("the monitor holds %d lines, over the %d-line tail; it is copying history", got, monitorTail)
	}
	body := conversationText(m)
	if !strings.Contains(body, "post 499") {
		t.Fatalf("the monitor does not show the newest line:\n%s", body)
	}
	if strings.Contains(body, "pre 0") {
		t.Fatalf("the monitor still shows the oldest line, so it kept every line rather than a tail")
	}
}
