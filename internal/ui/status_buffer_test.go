package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// In a grouped sidebar the server row is the network's own status buffer.
//
// A server is a connection, and its status buffer is where that connection talks: connection
// notices, the motd, errors, the things nobody asked for. Drawing a heading and then an indented
// "status" underneath it is two rows for one thing, and the one underneath is the row that never
// gets looked at - a user reading the sidebar sees "liberia" and a channel, and has to guess that
// the status buffer is somewhere else, or that there is one at all.
//
// Weechat settles it the same way: the server row is the buffer.
//
// These tests pin the shape, because it is the kind of layout decision that is trivially easy to
// undo while fixing something else - the heading is a heading again, the status window reappears
// underneath it, and nothing fails.

// twoNetworks is a model with two servers, each with a status window and one channel.
func twoNetworks(t *testing.T) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 120, Height: 24})
	m, _ = m.Step(Incoming(Line{Window: "status", Nick: "s", Text: "001", Kind: LineSystem, Network: "libera"}))
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Nick: "a", Text: "hi", Kind: LineMessage, Network: "libera"}))
	m, _ = m.Step(Incoming(Line{Window: "status", Nick: "s", Text: "001", Kind: LineSystem, Network: "oftc"}))
	m, _ = m.Step(Incoming(Line{Window: "#dev", Nick: "b", Text: "yo", Kind: LineMessage, Network: "oftc"}))
	return m
}

// TestTheServerRowIsTheStatusBuffer: one row per server, not a heading plus a status row.
func TestTheServerRowIsTheStatusBuffer(t *testing.T) {
	m := twoNetworks(t)
	rows := m.sidebarRows(12)

	statusRows := 0
	for _, row := range rows {
		if winName(row.window) == "status" {
			statusRows++
		}
	}
	if statusRows != 2 {
		t.Fatalf("the sidebar holds %d status rows, want one per server:\n%s", statusRows, dumpRows(rows))
	}
	// And each one is the heading of its own group, not an indented child of it.
	for _, row := range rows {
		if winName(row.window) == "status" {
			if !row.heading {
				t.Errorf("status row %q is not drawn as its server's row:\n%s", row.text, dumpRows(rows))
			}
			if !row.status {
				t.Errorf("status row %q is not marked as a status buffer; a plain heading cannot be selected", row.text)
			}
			// The label is the server name, optionally behind its buffer number ("2 libera"):
			// the number belongs to the row because /b2 has to reach it like any other window.
			if !strings.HasSuffix(row.text, m.networkOf(row.window)) {
				t.Errorf("the server row is labelled %q but selects the window of %q",
					row.text, m.networkOf(row.window))
			}
		}
	}
}

// TestClickingTheServerRowOpensItsStatus: the row is a window, so it must be clickable as one.
// A heading that is only a label is correctly unclickable; this one is not that.
func TestClickingTheServerRowOpensItsStatus(t *testing.T) {
	m := twoNetworks(t)

	// Row 0 of the content area is the first server row.
	got := m.WindowAt(3, 1)
	if winName(got) != "status" {
		t.Fatalf("clicking the first sidebar row selected %q, want the first server's status", got)
	}
	// And the channel underneath it is the next row, not the same one.
	next := m.WindowAt(3, 2)
	if next == got {
		t.Errorf("the server row and the row under it both select %q", got)
	}
	// The channel under the server is keyed by its network, which is what makes the two
	// networks' channels distinguishable at all.
	if want := "libera/#salotto"; next != want {
		t.Errorf("the row under the server selects %q, want %q", next, want)
	}
}

// TestSwitchingToTheServerRowMakesItActive covers the property that made this worth doing: the
// server row is a real buffer, so selecting it has to move the cursor and light it up.
func TestSwitchingToTheServerRowMakesItActive(t *testing.T) {
	m := twoNetworks(t)
	var serverKey string
	for _, row := range m.sidebarRows(12) {
		if row.status && m.networkOf(row.window) == "oftc" {
			serverKey = row.window
		}
	}
	if serverKey == "" {
		t.Fatal("no server row for oftc")
	}
	m = m.SetActive(serverKey)
	if m.active != serverKey {
		t.Fatalf("Selecting %q left the active window at %q", serverKey, m.active)
	}
	// The row must be drawn as selected - the marker, not just the window key changing.
	var sawMarker bool
	for _, line := range m.windowColumn(20, 12) {
		plain := stripANSI(line)
		if strings.Contains(plain, "▸") && strings.Contains(plain, "oftc") {
			sawMarker = true
		}
	}
	if !sawMarker {
		t.Errorf("the selected server row carries no marker:\n%s", strings.Join(m.windowColumn(20, 12), "\n"))
	}
}

// TestAWindowCalledStatusIsNotLostWhenTheServerRowTakesItsPlace: the merge must be a redraw of
// the same window, not a new row that shadows it. If the status window were dropped from the
// model, its unread count and its selection would go with it.
func TestTheStatusWindowIsReachableFromTheSidebar(t *testing.T) {
	m := twoNetworks(t)
	m, _ = m.Step(Incoming(Line{Window: "status", Nick: "s", Text: "a network notice", Kind: LineSystem, Network: "oftc"}))

	// The window must still exist - merging it into the server row is a redraw, not a deletion,
	// or its lines and its unread count would go with it.
	found := false
	for _, key := range m.order {
		if m.networkOf(key) == "oftc" && winName(key) == "status" {
			found = true
			if len(m.windows[key]) == 0 {
				t.Errorf("the status window of oftc holds no lines")
			}
		}
	}
	if !found {
		t.Fatalf("the status window of oftc is gone from the model: %v", m.order)
	}

	// And the sidebar must reach it. A window that exists and cannot be selected is a window the
	// user cannot open, which is worse than one that is drawn in the wrong place.
	reachable := false
	for _, row := range m.sidebarRows(12) {
		if m.networkOf(row.window) == "oftc" && winName(row.window) == "status" {
			reachable = true
		}
	}
	if !reachable {
		t.Fatalf("no sidebar row selects the status window of oftc:\n%s", dumpRows(m.sidebarRows(12)))
	}
}

func dumpRows(rows []sidebarRow) string {
	var b strings.Builder
	for i, row := range rows {
		b.WriteString("  ")
		b.WriteString(itoa(i))
		b.WriteString(" win=")
		b.WriteString(row.window)
		b.WriteString(" head=")
		b.WriteString(itoa(int(boolToInt(row.heading))))
		b.WriteString(" status=")
		b.WriteString(itoa(int(boolToInt(row.status))))
		b.WriteString(" text=")
		b.WriteString(row.text)
		b.WriteString("\n")
	}
	return b.String()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		return "-" + string(d)
	}
	return string(d)
}

// TestTheServerRowIsTheTopOfItsGroup reads the drawn sidebar, not the row list: the indent is applied
// by the renderer, so a row that says heading=true and still comes out two spaces in is a server that
// looks like one of its own channels.
//
// The comparison uses an INACTIVE server row on purpose. The active row's marker is "\u25b8 ", which
// occupies the same two cells as two blanks, so a test written on the selected row cannot tell an
// indent from a marker - and the indent mutation would survive it.
func TestTheServerRowIsTheTopOfItsGroup(t *testing.T) {
	m := twoNetworks(t)
	// libera is active; oftc (added second) is not, so its row is drawn with blanks.
	m = m.SetActive(m.keyFor("status"))
	lines := m.windowColumn(24, 12)

	// indentAfterMarker counts the blanks that follow the two-cell marker column.
	indentAfterMarker := func(label string) int {
		for _, line := range lines {
			plain := stripANSI(line)
			i := strings.Index(plain, label)
			if i < 2 {
				continue
			}
			rest := plain[2:i]
			return len(rest) - len(strings.TrimLeft(rest, " "))
		}
		return -1
	}
	server := indentAfterMarker("oftc")
	channel := indentAfterMarker("#dev")
	if server < 0 || channel < 0 {
		t.Fatalf("could not find the inactive server and its channel in:\n%s", strings.Join(lines, "\n"))
	}
	if server >= channel {
		t.Errorf("the server row is indented %d and its channel %d past the marker; a server is "+
			"the top of its group, not a child of it:\n%s", server, channel, strings.Join(lines, "\n"))
	}
}
