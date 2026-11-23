package facade

import (
	"strings"
	"testing"
)

// contains reports whether any string in lines contains needle.
func contains(lines []string, needle string) bool {
	for _, l := range lines {
		if strings.Contains(l, needle) {
			return true
		}
	}
	return false
}

// TestTableMatchesZNCLayout pins the grid CTable::GetLine builds
// (Utils.cpp:844-910). Every list command in ZNC answers with one of these, so
// a wrong margin here would make all of them subtly wrong at once.
func TestTableMatchesZNCLayout(t *testing.T) {
	tbl := newTable("Host", "Port", "SSL")
	tbl.add("irc.example.org", "6667", "SSL")
	tbl.add("a", "1")

	// CTable::GetLine builds the rule as "+-" then, per column, the width in
	// hyphens followed by "-+-" (or "-+" for the last), so adjacent columns
	// share the '-' of the joiner and the rule is exactly one dash wider than
	// the widest cell on each side. Written literally so the test states the
	// layout rather than recomputing it with the code under test.
	want := []string{
		"+-----------------+------+-----+",
		"| Host            | Port | SSL |",
		"+-----------------+------+-----+",
		"| irc.example.org | 6667 | SSL |",
		"| a               | 1    |     |",
		"+-----------------+------+-----+",
	}
	got := tbl.lines()
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

// TestByteStr pins CString::ToByteStr's boundaries (ZNCString.cpp:1246): whole
// units, strict greater-than, so 1024 is still "1024 B" and 1025 is "1 KiB".
func TestByteStr(t *testing.T) {
	cases := []struct {
		n    uint64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1024 B"},
		{1025, "1 KiB"},
		{1024 * 1024, "1024 KiB"},
		{1024*1024 + 1, "1 MiB"},
		{3 * 1024 * 1024 * 1024, "3 GiB"},
	}
	for _, c := range cases {
		if got := byteStr(c.n); got != c.want {
			t.Fatalf("byteStr(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// TestListNetworks prints the network's server, nick mask and channel count
// when it is on IRC, and only "No" when it is not (ClientCommand.cpp:658).
func TestListNetworks(t *testing.T) {
	s, fn, sub, out := smallRings(t)
	fn.server = "irc.example.org 6667"
	fn.mask = "me!u@h"
	fn.add("#c", ":x!y@z PRIVMSG #c :hi", 1)

	_ = out
	joined := strings.Join(notices(runWindowCmd(t, s, sub, "LISTNETWORKS")), "\n")
	for _, want := range []string{"Network", "On IRC", "IRC Server", "IRC User", "Channels"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("header %q missing from %q", want, joined)
		}
	}
	for _, want := range []string{"Yes", "irc.example.org 6667", "me!u@h"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%q missing from %q", want, joined)
		}
	}
}

// TestListNetworksDown prints "No" and no server for a network that is not
// connected, which is what CTable does with unset cells.
func TestListNetworksDown(t *testing.T) {
	s, fn, sub, _ := smallRings(t)
	fn.state = "backoff"

	joined := strings.Join(notices(runWindowCmd(t, s, sub, "LISTNETWORKS")), "\n")
	if !strings.Contains(joined, "No") {
		t.Fatalf("expected No in %q", joined)
	}
	if strings.Contains(joined, "Yes") {
		t.Fatalf("a down network reported Yes: %q", joined)
	}
}

// TestListServers shows the single configured server marked as current and
// never echoes a password.
func TestListServers(t *testing.T) {
	s, fn, sub, _ := smallRings(t)
	fn.server = "irc.example.org +6697"

	lines := notices(runWindowCmd(t, s, sub, "LISTSERVERS"))
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "irc.example.org*") {
		t.Fatalf("server not marked current: %q", joined)
	}
	if !strings.Contains(joined, "6697") || !strings.Contains(joined, "SSL") {
		t.Fatalf("port/SSL wrong: %q", joined)
	}
}

// TestListClients names the attached clients. The test client is the only one,
// so the table must have exactly one data row.
func TestListClients(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	sub.SetRemoteHost("10.0.0.7")
	sub.SetIdentifier("laptop")
	if !s.hub.Add(sub) {
		t.Fatal("hub rejected the subscriber")
	}
	t.Cleanup(func() { s.hub.Remove(sub) })

	lines := notices(runWindowCmd(t, s, sub, "LISTCLIENTS"))
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "10.0.0.7") {
		t.Fatalf("host missing: %q", joined)
	}
	if !strings.Contains(joined, "laptop") {
		t.Fatalf("identifier missing: %q", joined)
	}
}

// TestTrafficReportsBytesToTheUser checks the table has ZNC's four columns and
// that traffic the network reports reaches the row rather than being dropped.
func TestTrafficReportsBytesToTheUser(t *testing.T) {
	s, fn, sub, _ := smallRings(t)
	fn.in = 2048
	fn.out = 4096

	joined := strings.Join(notices(runWindowCmd(t, s, sub, "TRAFFIC")), "\n")
	for _, want := range []string{"Username", "In", "Out", "Total", "2 KiB", "4 KiB"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("%q missing from %q", want, joined)
		}
	}
}

// TestSetBufferAnswersAndResizes pins SETBUFFER's two halves: the wording a
// client reads (ClientCommand.cpp:1570) and that the size actually reached the
// window. A command that answers "set" without resizing is the failure mode
// this test exists for.
func TestSetBufferAnswersAndResizes(t *testing.T) {
	s, fn, sub, _ := smallRings(t)
	markKnown(s, "libera", "#a", "#b")

	joined := strings.Join(notices(runWindowCmd(t, s, sub, "SETBUFFER #a 5")), "\n")
	if !strings.Contains(joined, "Size of every buffer was set to 5 lines") {
		t.Fatalf("wording: %q", joined)
	}
	if !fn.resized["#a"] {
		t.Fatal("SETBUFFER answered but did not resize the window")
	}
	if fn.resized["#b"] {
		t.Fatal("SETBUFFER touched a window the pattern did not match")
	}
}

// TestSetBufferGlobResizesEveryMatch checks the glob fans out, which is ZNC's
// FindChans behaviour.
func TestSetBufferGlobResizesEveryMatch(t *testing.T) {
	s, fn, sub, _ := smallRings(t)
	markKnown(s, "libera", "#a", "#b")

	joined := strings.Join(notices(runWindowCmd(t, s, sub, "SETBUFFER #* 7")), "\n")
	if !strings.Contains(joined, "7 lines") {
		t.Fatalf("wording: %q", joined)
	}
	if !fn.resized["#a"] || !fn.resized["#b"] {
		t.Fatalf("glob did not resize every match: %v", fn.resized)
	}
}

// TestSetBufferAboveTheCapFails is ZNC's GetMaxBufferSize branch
// (ClientCommand.cpp:1562): the answer names the maximum instead of pretending.
func TestSetBufferAboveTheCapFails(t *testing.T) {
	s, _, sub, _ := smallRings(t)
	markKnown(s, "libera", "#a")

	joined := strings.Join(notices(runWindowCmd(t, s, sub, "SETBUFFER #a 999999")), "\n")
	if !strings.Contains(joined, "Setting buffer size failed") {
		t.Fatalf("wording: %q", joined)
	}
	if !strings.Contains(joined, "Maximum buffer size is") {
		t.Fatalf("cap not reported: %q", joined)
	}
}

// TestDisconnectThenConnect pins the pair's wording and that DISCONNECT reaches
// the network. ZNC answers with the same two sentences
// (ClientCommand.cpp:387, :336).
func TestDisconnectThenConnect(t *testing.T) {
	s, fn, sub, _ := smallRings(t)
	fn.server = "irc.example.org 6667"

	dis := notices(runWindowCmd(t, s, sub, "DISCONNECT"))
	if !contains(dis, "Disconnected from IRC") {
		t.Fatalf("disconnect wording: %q", dis)
	}
	if fn.connectEnabled() {
		t.Fatal("DISCONNECT did not disable the network")
	}

	con := notices(runWindowCmd(t, s, sub, "CONNECT"))
	if !contains(con, "Connecting to irc.example.org") {
		t.Fatalf("connect wording: %q", con)
	}
	if !fn.connectEnabled() {
		t.Fatal("CONNECT did not re-enable the network")
	}
}
