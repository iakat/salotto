package ui

import (
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// These tests are the answer to "can you replicate this yourself": they render the frame the way a
// real session does and assert on what a person looking at the screen would see. The earlier gap
// was not that the code was untested - it is that the tests asserted on text the renderer produces
// rather than on the shape of the screen, so a sidebar with no heading, a frame one cell narrow and
// a scrollback that scrolls into emptiness all passed.
//
// A frame is one box: every row exactly as wide as the terminal. A row one cell short looks fine
// until the next row's corner meets it, and then the right-hand edge is a staircase.

// sessionModel is a model in the state a real session is in: a network that is known, a channel
// with traffic, a topic, a member list and a status window.
//
// It is built through the same messages the transport sends, because a model assembled by poking
// fields is a model that can be internally inconsistent in a way no real client ever is - and the
// bugs on screen were all of the second kind: state that existed under one name and was looked up
// under another.
func sessionModel(t *testing.T, width, height int) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: width, Height: height})
	m, _ = m.Step(Incoming(Line{Window: "status", Nick: "server", Text: "001 nicholas :Welcome", Kind: LineSystem, Network: "libera"}))
	for i := 0; i < 30; i++ {
		m, _ = m.Step(Incoming(Line{Window: "#salotto", Nick: "alice", Text: "a line of conversation", Kind: LineMessage, Network: "libera"}))
	}
	m, _ = m.Step(Topic{Window: "#salotto", Text: "a topic about salotto"})
	m, _ = m.Step(Nicklist{Window: "#salotto", Nicks: []Nick{{Name: "alice", Prefix: "@"}, {Name: "bob"}}, Network: "libera"})
	m = m.SetActive(m.keyFor("#salotto"))
	return m
}

// rowsOf renders the model and returns its rows, escape sequences removed.
//
// Stripped because every measurement here is about what a person sees: an ANSI sequence is zero
// cells wide, but it is several characters long, so a scan for "│" or for a window name finds the
// colour codes instead and reports a defect that is not on screen.
func rowsOf(m Model) []string {
	raw := strings.Split(m.View(), "\n")
	out := make([]string, len(raw))
	for i, row := range raw {
		out[i] = ansiSeq.ReplaceAllString(row, "")
	}
	return out
}

var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

// TestEveryRowIsExactlyAsWideAsTheTerminal covers the whole frame at the sizes a client is used at,
// with and without the optional columns. The reported defect - the top-right corner and the input
// row's right pipe one cell left of everything else - is exactly what this measures.
func TestEveryRowIsExactlyAsWideAsTheTerminal(t *testing.T) {
	for _, width := range []int{72, 80, 100, 120, 160, 200, 201, 202, 240} {
		for _, height := range []int{20, 30} {
			m := sessionModel(t, width, height)
			for i, row := range rowsOf(m) {
				if row == "" {
					continue
				}
				if got := lipgloss.Width(row); got != width {
					t.Errorf("%dx%d: row %d is %d cells, want %d\n  %q", width, height, i, got, width, row)
				}
			}
		}
	}
}

// TestTheChannelIsUnderItsServerAndNotUnderStatus is the reported sidebar defect: "#salotto is
// under status but it should be under network name", and "status is on the top left twice".
//
// A window's network comes from its key. The bare "status" window exists before any line has named
// a server, and the moment a tagged line arrives it is rekeyed to "libera/status" - but if the
// rekey leaves the old key behind, or leaves it without a network, the sidebar has a loose window
// that is drawn ungrouped and appears to belong to whatever heading precedes it.
func TestTheChannelIsUnderItsServerAndNotUnderStatus(t *testing.T) {
	m := sessionModel(t, 120, 24)

	// Every window the model holds must know its network, or the sidebar cannot place it.
	for _, key := range m.order {
		if net := m.networkOf(key); net == "" {
			t.Errorf("window %q has no network, so it cannot be grouped", key)
		}
	}
	// And there must be exactly one status window, not one bare and one qualified.
	statusCount := 0
	for _, key := range m.order {
		if winName(key) == "status" {
			statusCount++
		}
	}
	if statusCount != 1 {
		t.Errorf("the sidebar holds %d status windows: %v", statusCount, m.order)
	}

	// The channel must be drawn under the network heading, not above it. Only the sidebar column
	// is read: the title row and the status bar also mention the network and the channel, and a
	// scan of the whole row finds those first and reports a grouping defect that is not there.
	rows := rowsOf(m)
	sidebar := func(row string) string {
		// The sidebar is everything between the frame's left pipe and the next pipe.
		parts := strings.Split(row, "│")
		if len(parts) < 2 {
			return ""
		}
		return parts[1]
	}
	headingRow, channelRow := -1, -1
	for i, row := range rows {
		col := sidebar(row)
		if strings.Contains(col, "libera") && headingRow < 0 {
			headingRow = i
		}
		if strings.Contains(col, "#salotto") && channelRow < 0 {
			channelRow = i
		}
	}
	if headingRow < 0 {
		t.Fatalf("no server heading in the sidebar; it is flat:\n%s", strings.Join(rows, "\n"))
	}
	if channelRow < 0 {
		t.Fatalf("the channel is not in the sidebar at all:\n%s", strings.Join(rows, "\n"))
	}
	if channelRow < headingRow {
		t.Errorf("the channel is drawn at row %d, above the server heading at row %d", channelRow, headingRow)
	}
}

// TestTheSidebarShowsEachWindowOnce: "status is on the top left twice". A window drawn in two groups
// is a window with two identities, and the client then has to guess which one a click means.
func TestTheSidebarShowsEachWindowOnce(t *testing.T) {
	m := sessionModel(t, 120, 24)
	rows := rowsOf(m)
	seen := map[string]int{}
	for _, row := range rows {
		parts := strings.Split(row, "│")
		if len(parts) < 2 {
			continue
		}
		label := strings.TrimSpace(parts[1])
		// Strip the buffer number and the active marker, leaving the window's name.
		label = strings.TrimPrefix(label, "▸ ")
		if i := strings.IndexByte(label, ' '); i >= 0 && i < 3 {
			label = strings.TrimSpace(label[i+1:])
		}
		if label == "" {
			continue
		}
		seen[label]++
	}
	for label, n := range seen {
		if n > 1 && label != "libera" {
			t.Errorf("%q appears %d times in the sidebar; each window is one row", label, n)
		}
	}
}

// TestTheMemberListIsDrawn: "there's no nick list". The member list is a column inside the frame,
// so its absence is a layout decision going wrong, not a missing feature.
func TestTheMemberListIsDrawn(t *testing.T) {
	m := sessionModel(t, 140, 24)

	if len(m.nicks[m.active]) == 0 {
		t.Fatalf("the model holds no member list for the active window %q; it holds keys %v",
			m.active, keysOfNicks(m.nicks))
	}
	view := m.View()
	if !strings.Contains(view, "alice") || !strings.Contains(view, "bob") {
		t.Errorf("the member list is not on screen:\n%s", view)
	}
	// A nick column is separated from the conversation by a pipe; without one there is no column.
	if strings.Count(strings.Split(view, "\n")[3], "│") < 3 {
		t.Errorf("no member column in the frame's third row:\n%q", strings.Split(view, "\n")[3])
	}
}

// TestTheTopicRowIsDrawn: "no topic row shows up, it should". A topic is a row of its own under the
// title, spanning the frame.
func TestTheTopicRowIsDrawn(t *testing.T) {
	m := sessionModel(t, 140, 24)

	// The topic is stored under the window key; a topic filed under the bare name while the
	// conversation is filed under the qualified one is a topic that exists and is never drawn.
	if got := m.topics[m.active]; got != "a topic about salotto" {
		t.Fatalf("the topic is stored under %q, not the active key %q; topics are %v",
			got, m.active, keysOfStrings(m.topics))
	}
	if !strings.Contains(m.View(), "a topic about salotto") {
		t.Errorf("the topic is not on screen:\n%s", m.View())
	}
}

// TestScrollingUpStopsAtTheTop: "scrolling too far up still results in empty scrollback". Scrolling
// past the oldest line must clamp at the oldest line, not walk off the end into blank space.
func TestScrollingUpStopsAtTheTop(t *testing.T) {
	m := sessionModel(t, 120, 20)
	// Scroll far further than the buffer is long - a wheel held down, or a held key.
	for i := 0; i < 200; i++ {
		m = m.scrollBy(10)
	}
	if m.scroll > len(m.windows[m.active]) {
		t.Fatalf("scrolled to %d in a window of %d lines: the view is past the oldest line",
			m.scroll, len(m.windows[m.active]))
	}
	// And what is on screen must contain a real line, not only blanks.
	body := strings.Join(rowsOf(m)[2:18], "\n")
	if strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(body, "│", ""), " ", "")) == "" {
		t.Fatalf("the scrollback is empty after scrolling to the top:\n%s", strings.Join(rowsOf(m), "\n"))
	}
}

func keysOfNicks(m map[string][]Nick) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keysOfStrings(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
