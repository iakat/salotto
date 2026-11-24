package ui

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The sidebar is grouped by server and every buffer carries a weechat-style number. The two
// arrived together and are tested together, because the number is what a grouped list is
// navigated with: a heading you cannot type at is decoration, and a number that renumbers when
// a server appears is worse than no number.

// sidebarFixture is two networks with a window each, plus the status window on one of them.
// freenode sorts before libera, so the expected order is fixed and the test can be exact about
// it instead of "somewhere in the output".
func sidebarFixture(t *testing.T) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Text: "connected", Kind: LineSystem}))
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "libera", Nick: "alice", Text: "ciao", Kind: LineMessage}))
	m, _ = m.Step(Incoming(Line{Window: "#other", Network: "freenode", Nick: "bob", Text: "hi", Kind: LineMessage}))
	// The two quiet channels still count as unread, because they arrived while the user was in
	// status. Cleared here so the layout tests below assert on layout: the badge has its own
	// test, and a count in the middle of a string comparison is noise that hides the real
	// difference.
	m.unread = make(map[string]int)
	return m
}

// sidebarText is the window column as plain rows, without ANSI and without padding, so an
// assertion is about the list and not about the styling.
func sidebarText(m Model) []string {
	l := m.layout()
	col := m.windowColumn(l.winWidth, l.contentRows)
	out := make([]string, 0, len(col))
	for _, row := range col {
		out = append(out, strings.TrimRight(plainText(row), " "))
	}
	return out
}

// TestTheSidebarGroupsWindowsUnderTheirServer is the user's first request: "it should be
// ordered the channel list and be under server name".
//
// Exact rows, not a search: a heading per server and a window under the right one is the whole
// feature, and an assertion that only looked for "#salotto" somewhere on screen would pass with
// the window filed under the wrong server.
func TestTheSidebarGroupsWindowsUnderTheirServer(t *testing.T) {
	m := sidebarFixture(t)
	got := sidebarText(m)
	// Two spaces of indent under the heading, then the marker column: an inactive row is four
	// cells in, an active one has its arrow in the marker column so the arrow and the names line
	// up as two ranks of the same list.
	want := []string{
		"freenode",
		"    3 #other",
		"libera",
		"    2 #salotto",
		"  ▸ 1 status",
	}
	if len(got) < len(want) {
		t.Fatalf("the sidebar drew %d rows, want at least %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("sidebar row %d is %q, want %q:\n%s", i, got[i], w, strings.Join(got, "\n"))
		}
	}
}

// TestGroupingCanBeTurnedOff returns the old flat list, with the numbers still there.
//
// The flat list is what the client looked like before, so it is the fallback for someone who
// learnt the old layout - and it is a setting, not a second window list, because two layouts
// drawn by two functions is the drift this package exists to avoid.
func TestGroupingCanBeTurnedOff(t *testing.T) {
	m := sidebarFixture(t)
	m.settings.GroupByServer = false
	got := sidebarText(m)
	want := []string{"▸ 1 status", "  2 #salotto", "  3 #other"}
	if len(got) < len(want) {
		t.Fatalf("the flat list drew %d rows, want at least %d:\n%s", len(got), len(want), strings.Join(got, "\n"))
	}
	for i, w := range want {
		if got[i] != w {
			t.Fatalf("flat row %d is %q, want %q:\n%s", i, got[i], w, strings.Join(got, "\n"))
		}
	}
	for _, row := range got {
		if !strings.Contains(row, "1 ") && !strings.Contains(row, "2 ") && !strings.Contains(row, "3 ") {
			t.Fatalf("a row without its buffer number survived the layout change: %q", row)
		}
	}
}

// TestBufferNumbersCanBeTurnedOff: a number is a column, and one column of a sixteen-column
// sidebar is a long channel name truncated for nothing if the user does not use the numbers.
func TestBufferNumbersCanBeTurnedOff(t *testing.T) {
	m := sidebarFixture(t)
	m.settings.BufferNumbers = false
	got := sidebarText(m)
	for _, row := range got {
		for _, digit := range []string{"1 ", "2 ", "3 "} {
			if strings.Contains(row, "▸ "+digit) || strings.Contains(row, "  "+digit) {
				t.Fatalf("numbers are still drawn with the setting off: %q", row)
			}
		}
	}
	// And /b still works: the number is identity, and turning off the label must not turn off
	// the thing the label was showing.
	if after := typeLine(m, "/b 3"); winName(after.active) != "#other" {
		t.Fatalf("/b 3 landed on %q with the numbers hidden from the sidebar", after.active)
	}
}

// TestABufferKeepsItsNumber is the stability property, and it is the one a user notices when it
// is broken: they type /b 3 from memory and land in somebody's query window.
//
// The other windows appear *between* the two lookups on purpose. A numbering derived from
// position passes the first assertion and fails the second, which is exactly the bug this
// guards against.
func TestABufferKeepsItsNumber(t *testing.T) {
	m := sidebarFixture(t)
	before := m.bufferNo[m.keyFor("#salotto")]
	if before == 0 {
		t.Fatal("#salotto has no buffer number")
	}

	for _, name := range []string{"#later-one", "#later-two"} {
		m, _ = m.Step(Incoming(Line{Window: name, Network: "libera", Nick: "x", Text: "hi", Kind: LineMessage}))
	}
	if after := m.bufferNo[m.keyFor("#salotto")]; after != before {
		t.Fatalf("#salotto was number %d and became %d when other windows arrived", before, after)
	}

	// No two windows share a number, and every window has one.
	seen := make(map[int]string, len(m.windows))
	for name := range m.windows {
		n := m.bufferNo[name]
		if n <= 0 {
			t.Fatalf("%q has no buffer number", name)
		}
		if other, dup := seen[n]; dup {
			t.Fatalf("%q and %q share buffer number %d", name, other, n)
		}
		seen[n] = name
	}
}

// TestAWindowOpeningCannotStealANumber covers the bouncer's half of the same property: a window
// that arrives from upstream gets a fresh number, never one that is already in use, however
// many arrive or in whatever order.
func TestAWindowOpeningCannotStealANumber(t *testing.T) {
	m := sidebarFixture(t)
	held := make(map[int]string, len(m.bufferNo))
	for name, n := range m.bufferNo {
		held[n] = name
	}
	for i := 0; i < 20; i++ {
		m, _ = m.Step(Incoming(Line{Window: "#bulk" + strconv.Itoa(i), Network: "libera", Text: "x", Kind: LineMessage}))
	}
	for name, n := range m.bufferNo {
		if prev, ok := held[n]; ok && prev != name {
			t.Fatalf("%q took number %d, which belongs to %q", name, n, prev)
		}
		held[n] = name
	}
}

// TestSlashBSwitchesByNumber is the command the numbers exist for.
//
// Both spellings, because both are in muscle memory, and a bare /b lists rather than doing
// nothing: the sidebar can be off, and a command whose whole interface is a number has to say
// which numbers there are.
func TestSlashBSwitchesByNumber(t *testing.T) {
	m := sidebarFixture(t)
	m = m.SetActive("status")

	if after := typeLine(m, "/b 3"); winName(after.active) != "#other" {
		t.Fatalf("/b 3 landed on %q, want #other", after.active)
	}
	m = typeLine(m, "/buffer 2")
	if winName(m.active) != "#salotto" {
		t.Fatalf("/buffer 2 landed on %q, want #salotto", m.active)
	}
	// A number nobody holds is refused by name: a silent no-op reads as a broken key.
	after := typeLine(m, "/b 99")
	if winName(after.active) != "#salotto" {
		t.Fatalf("/b 99 switched to %q", after.active)
	}
	if reason := systemText(after); !strings.Contains(reason, "no buffer 99") {
		t.Fatalf("/b 99 said %q", reason)
	}
	// And a bare /b lists the numbers.
	if list := systemText(typeLine(m, "/b")); !strings.Contains(list, "3 #other") {
		t.Fatalf("/b did not list the buffers: %q", list)
	}
}

// TestABufferNumberIsNotReusedAfterClose: the number of a closed window is retired. Reusing it
// is how a user types /b 3 and lands in a window they have never seen, which is the opposite of
// what a stable number is for.
func TestABufferNumberIsNotReusedAfterClose(t *testing.T) {
	m := sidebarFixture(t)
	closed := m.bufferNo[m.keyFor("#other")]
	m = m.SetActive("#other")
	m = typeLine(m, "/close")

	m, _ = m.Step(Incoming(Line{Window: "#fresh", Network: "freenode", Text: "x", Kind: LineMessage}))
	if got := m.bufferNo[m.keyFor("#fresh")]; got == closed {
		t.Fatalf("#fresh took %d, the number of the window that just closed", closed)
	}
	// And the retired number does not reopen the closed window.
	if after := typeLine(m, "/b "+strconv.Itoa(closed)); winName(after.active) == "#other" {
		t.Fatalf("/b %d reopened a closed window", closed)
	}
}

// TestAWindowWithNoKnownNetworkStillRenders is the fallback, and it is not an edge case: a
// client connected straight to an IRCd has no networks at all, and a bouncer whose tag path
// broke must not hide every channel behind a grouping it cannot do.
func TestAWindowWithNoKnownNetworkStillRenders(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "#lonely", Text: "nobody told me", Nick: "alice", Kind: LineMessage}))
	// It arrived while the user was in status, so clear the count: this test is about the row
	// existing and carrying its number, not about the badge.
	m.unread = make(map[string]int)

	rows := sidebarText(m)
	joined := strings.Join(rows, "\n")
	if !strings.Contains(joined, "#lonely") {
		t.Fatalf("a window with no network is not drawn:\n%s", joined)
	}
	if !strings.Contains(joined, "▸ 1 status") {
		t.Fatalf("the active window lost its marker or number:\n%s", joined)
	}
	// No headings at all: with nothing to group by, the flat list is the honest answer.
	if len(rows) != 2 {
		t.Fatalf("a client with no networks drew %d rows for 2 windows:\n%s", len(rows), joined)
	}
	// The channel that arrived is numbered like everything else, so /b can reach it even
	// though no server ever told us which network it is on.
	if !strings.Contains(joined, "2 #lonely") {
		t.Fatalf("the ungrouped window has no buffer number:\n%s", joined)
	}
}

// TestTheTransportReadsTheNetworkTag: the tag the bouncer adds is the only place a client can
// learn which server a window is on, so the parsing is asserted on what comes out of the
// transport rather than on the tag parser alone. A tag that parses and is then dropped leaves
// the sidebar flat with no error anywhere.
func TestTheTransportReadsTheNetworkTag(t *testing.T) {
	tr := newTestTransport(t)
	tr.handle([]byte("@salotto.network=libera;time=2026-11-24T06:08:08.680Z :n!u@h PRIVMSG #c :hello"))

	inc, ok := nextIncoming(t, tr)
	if !ok {
		t.Fatal("nothing came out of the transport")
	}
	if inc.Network != "libera" {
		t.Fatalf("the line's network is %q, want libera", inc.Network)
	}
	if inc.Window != "#c" {
		t.Fatalf("a tagged line landed in %q", inc.Window)
	}

	// The escapes matter: a network name is free text from a config file, and a space in it
	// must not end the tag blob - which would make the client read the verb out of the name.
	tr.handle([]byte("@salotto.network=my\\snet :n!u@h PRIVMSG #c :hello again"))
	if inc, ok := nextIncoming(t, tr); !ok || inc.Network != "my net" {
		t.Fatalf("an escaped network name came through as %q", inc.Network)
	}

	// And no tag means no network, not a made-up one.
	tr.handle([]byte(":n!u@h PRIVMSG #c :untagged"))
	if inc, ok := nextIncoming(t, tr); !ok || inc.Network != "" {
		t.Fatalf("an untagged line was given the network %q", inc.Network)
	}
}

// TestTheNicklistCarriesTheNetworkItLearned: 353 arrives before any conversation in a fresh
// channel, so a client that only learned networks from messages would file a quiet channel
// under no heading until somebody spoke - which on a quiet channel is hours.
func TestTheNicklistCarriesTheNetworkItLearned(t *testing.T) {
	tr := newTestTransport(t)
	tr.noteNetwork("#c", "libera")
	tr.mu.Lock()
	tr.members["#c"] = map[string]string{"alice": ""}
	tr.mu.Unlock()
	tr.publishMembers("#c")

	for {
		msg, ok := <-tr.Msgs()
		if !ok {
			t.Fatal("the transport closed")
		}
		nl, ok := msg.(Nicklist)
		if !ok {
			continue
		}
		if nl.Network != "libera" {
			t.Fatalf("the nicklist for #c carries network %q", nl.Network)
		}
		return
	}
}

// TestAnUngroupedWindowRidesAlongWithTheGroupedOnes: a mixed client - a bouncer with one
// network and a window that arrived without a tag - must still show both. The untagged one goes
// last rather than under the first heading, because filing it under a server it may not be on
// is a lie the user cannot see through.
func TestAnUngroupedWindowRidesAlongWithTheGroupedOnes(t *testing.T) {
	m := sidebarFixture(t)
	m, _ = m.Step(Incoming(Line{Window: "#orphan", Text: "x", Nick: "carol", Kind: LineMessage}))

	got := sidebarText(m)
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "#orphan") {
		t.Fatalf("the untagged window was dropped:\n%s", joined)
	}
	// It is below every heading and indented like the members of a group, so it reads as part
	// of the list rather than as a third server.
	last := got[len(got)-1]
	if !strings.Contains(last, "#orphan") {
		t.Fatalf("the untagged window is not at the end of the list:\n%s", joined)
	}
}

// TestHeadingsAreDroppedWhenTheyDoNotFit: a heading per server costs a row, and in a short
// terminal those rows are the windows. The rule is the one the sidebar already followed for
// narrow terminals - drop rather than clip - because a list that shows two servers and hides
// the channels under the second one is worse than the flat list it replaced.
func TestHeadingsAreDroppedWhenTheyDoNotFit(t *testing.T) {
	m := sidebarFixture(t)
	// Four rows of content: the flat list of three fits, the grouped list of five does not.
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 9})
	l := m.layout()
	if l.contentRows >= 5 || l.contentRows < 3 {
		t.Fatalf("setup: %d content rows is not the tight window this test needs", l.contentRows)
	}
	rows := m.sidebarRows(l.contentRows)
	for _, row := range rows {
		if row.heading {
			t.Fatalf("a heading was drawn in %d rows: %+v", l.contentRows, rows)
		}
	}
	// Every window, not the first contentRows of them: dropping the headings is meant to buy
	// the rows back for the windows, not to trade one truncation for another.
	if len(rows) != len(m.windows) {
		t.Fatalf("the flat list drew %d of %d windows in %d cells", len(rows), len(m.windows), l.contentRows)
	}
}

// TestClickingAHeadingSelectsNothing: a heading is not a window, so a click on it must do
// nothing at all. The alternative - it selects its first child - is a click that switches to a
// window the user did not point at.
//
// The rows below the heading are checked too, because the bug this guards against is the
// off-by-one: with the hit-test reading m.order, every row after the first heading selects its
// neighbour, and a click on #salotto opens #other.
func TestClickingAHeadingSelectsNothing(t *testing.T) {
	m := sidebarFixture(t)
	l := m.layout()

	var headingRow, salottoRow, otherRow = -1, -1, -1
	rows := m.sidebarRows(l.contentRows)
	for i, row := range rows {
		switch {
		case row.heading && row.text == "freenode":
			headingRow = i
		case winName(row.window) == "#salotto":
			salottoRow = i
		case winName(row.window) == "#other":
			otherRow = i
		}
	}
	if headingRow < 0 || salottoRow < 0 || otherRow < 0 {
		t.Fatalf("setup: rows not found in %+v", rows)
	}

	// WindowAt works in screen coordinates: row 0 is the top border.
	screen := func(row int) int { return row + 1 }
	if got := m.WindowAt(3, screen(headingRow)); got != "" {
		t.Fatalf("a click on the %q heading selected %q", "freenode", got)
	}
	if got := m.WindowAt(3, screen(salottoRow)); winName(got) != "#salotto" {
		t.Fatalf("a click on the #salotto row selected %q", got)
	}
	if got := m.WindowAt(3, screen(otherRow)); winName(got) != "#other" {
		t.Fatalf("a click on the #other row selected %q", got)
	}
}

// TestTheSidebarSettingsAreFsetAble: both are settings, so both must be settable and both must
// read back truthfully. A confirmation line that lies is worse than none, because it teaches the
// user to stop reading it.
func TestTheSidebarSettingsAreFsetAble(t *testing.T) {
	m := New("status")
	if !m.settings.GroupByServer || !m.settings.BufferNumbers {
		t.Fatal("setup: the two new settings do not default to on")
	}

	for _, key := range []string{"group_by_server", "buffer_numbers"} {
		m = typeLine(m, "/fset "+key+" off")
		if got := valueOf(m.settings, key); got != "off" {
			t.Fatalf("/fset %s off left it reading %q", key, got)
		}
		if !strings.Contains(systemText(m), key+" is now off") {
			t.Fatalf("/fset %s off did not confirm with its own value: %q", key, systemText(m))
		}
		m = typeLine(m, "/fset "+key+" on")
		if got := valueOf(m.settings, key); got != "on" {
			t.Fatalf("/fset %s on left it reading %q", key, got)
		}
	}

	// The aliases people will actually type, through the one resolver there is.
	for alias, want := range map[string]string{
		"servers": "group_by_server",
		"groups":  "group_by_server",
		"numbers": "buffer_numbers",
		"buffers": "buffer_numbers",
	} {
		if got := canonical(alias); got != want {
			t.Fatalf("canonical(%q) = %q, want %q", alias, got, want)
		}
	}
}

// TestAClientCommandIsOfferedByTab: the help text and Tab completion read modelCommands, and the
// dispatch does not. A command that resolves but is never offered is a command only its author
// knows, which is how /b would have shipped - the numbers in the sidebar are useless if nothing
// says how to reach them. Checked from both ends: it is in the table, and it answers locally.
func TestAClientCommandIsOfferedByTab(t *testing.T) {
	if !contains(modelCommands, "/b") {
		t.Fatalf("/b is resolved by the model but not in modelCommands (%v), so neither /help nor Tab mentions it", modelCommands)
	}
	if body := systemText(typeLine(New("status"), "/help")); !strings.Contains(body, "/b") {
		t.Fatalf("/help does not mention /b:\n%s", body)
	}
	// And it must not reach the wire: the numbers are the client's, and a bouncer has never
	// heard of them.
	if cmd := typeLineCmd(New("status"), "/b"); cmd != nil {
		if _, sent := cmd().(Sent); sent {
			t.Fatal("/b was forwarded to the bouncer; it is a client command")
		}
	}
}

// TestALineInTheActiveWindowStillNamesItsNetwork: a line that arrives without a window is drawn
// in the active one, so that is the window whose network it belongs to. The resolution happens
// once, in Step, because doing it only in append would leave the sidebar ungrouped for a window
// whose first line was the one that named its server - and status is that window on every
// connection.
func TestALineInTheActiveWindowStillNamesItsNetwork(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Network: "libera", Text: "connected", Kind: LineSystem}))

	if got := m.networkOf(m.keyFor("status")); got != "libera" {
		t.Fatalf("a line with no window left status on network %q, want libera", got)
	}
	rows := m.sidebarRows(m.layout().contentRows)
	if len(rows) == 0 || !rows[0].heading || rows[0].text != "libera" {
		t.Fatalf("status is not grouped under its server: %+v", rows)
	}
}

// TestAnUntaggedLineDoesNotUngroupAWindow: the network is learned once and kept. A relayed line,
// a gap marker, or a web frame rebuilt from JSON can arrive without the tag, and forgetting on
// those would move the channel the user is reading out from under its heading - a list that
// reshuffles while it is being read, which is the one thing a sidebar must never do.
func TestAnUntaggedLineDoesNotUngroupAWindow(t *testing.T) {
	m := sidebarFixture(t)
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Text: "no tag on this one", Nick: "alice", Kind: LineMessage}))
	if got := m.networkOf(m.keyFor("#salotto")); got != "libera" {
		t.Fatalf("an untagged line changed #salotto's network to %q", got)
	}
	// And it is still drawn under libera rather than in the ungrouped block at the end.
	rows := m.sidebarRows(m.layout().contentRows)
	for i, row := range rows {
		if winName(row.window) != "#salotto" {
			continue
		}
		if i == 0 || !rows[i-1].heading || rows[i-1].text != "libera" {
			t.Fatalf("#salotto is no longer under the libera heading: %+v", rows)
		}
		return
	}
	t.Fatalf("#salotto vanished from the sidebar: %+v", rows)
}

// TestTheTwoSidebarSettingsSurviveARestart: they are the client's appearance, and a restart is
// the moment a user finds out whether a setting was real. Losing them is a client that looks
// broken after every reconnect, which for the web front end is every page load.
func TestTheTwoSidebarSettingsSurviveARestart(t *testing.T) {
	path := t.TempDir() + "/settings.json"
	m := New("#test").WithSettings(path)
	m = typeLine(m, "/fset group_by_server off")
	m = typeLine(m, "/fset buffer_numbers off")

	reloaded := New("#test").WithSettings(path)
	if reloaded.settings.GroupByServer || reloaded.settings.BufferNumbers {
		t.Fatalf("the sidebar settings did not survive a restart: %+v", reloaded.settings)
	}
}
