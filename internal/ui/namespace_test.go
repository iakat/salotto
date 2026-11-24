package ui

import (
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Two networks, one window name: the namespace bug from QUEUE.md 2.1.1, as a test.
//
// Every network has a window called "status", and the bouncer keys its windows by
// (network, name). The client used to key by name alone, so both networks' registration
// replays landed in one window and whichever arrived last won - the other network's errors,
// MOTDs and bouncer replies were delivered by the bouncer and dropped on the floor here.
//
// The assertion is on the *lines*, not on the sidebar: a sidebar that draws two rows while
// the lines are merged is a display fix over a data-loss bug, which is exactly the mistake
// the earlier revision of this entry made. Both halves are checked, because they can break
// independently - the keys can separate while the renderer still shows one row.
func TestTwoNetworksKeepSeparateWindowsOfTheSameName(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "first", Nick: "srv", Text: "welcome to first", Kind: LineNotice}))
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "second", Nick: "srv", Text: "welcome to second", Kind: LineNotice}))

	// Two windows exist, and neither holds the other's line.
	if len(m.windows) != 2 {
		t.Fatalf("two networks' status windows became %d windows: %v", len(m.windows), m.order)
	}
	firstKey, secondKey := windowKey("first", "status"), windowKey("second", "status")
	for _, c := range []struct{ key, want, unwanted string }{
		{firstKey, "welcome to first", "welcome to second"},
		{secondKey, "welcome to second", "welcome to first"},
	} {
		lines := m.windows[c.key]
		if len(lines) == 0 {
			t.Fatalf("%q holds no lines; windows are %v", c.key, m.order)
		}
		var got strings.Builder
		for _, l := range lines {
			got.WriteString(l.Text)
			got.WriteString("\n")
		}
		if !strings.Contains(got.String(), c.want) {
			t.Fatalf("%q does not hold %q:\n%s", c.key, c.want, got.String())
		}
		if strings.Contains(got.String(), c.unwanted) {
			t.Fatalf("%q also holds the other network's line %q:\n%s", c.key, c.unwanted, got.String())
		}
	}

	// And the sidebar shows both, under their own headings, each reachable by number.
	rows := sidebarText(m)
	joined := strings.Join(rows, "\n")
	for _, want := range []string{"first", "second", "status"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the sidebar is missing %q:\n%s", want, joined)
		}
	}
	if a, b := m.bufferNo[firstKey], m.bufferNo[secondKey]; a == 0 || b == 0 || a == b {
		t.Fatalf("the two status windows got numbers %d and %d", a, b)
	}
}

// A window whose network is learnt late keeps its number, its scrollback and its place. This
// is the migration path: a client attaches, a bare "status" window exists with a welcome line
// in it, and only then does a tagged line reveal which server that is.
//
// Getting this wrong is the subtle one - the lines move to the qualified key but the buffer
// number does not, so /b 1 lands nowhere while the sidebar still shows "1 status".
func TestAWindowKeepsItsIdentityWhenItsNetworkIsLearnt(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	// Untagged first: no network known, so the window is on its bare name.
	m, _ = m.Step(Incoming(Line{Window: "status", Nick: "srv", Text: "an untagged welcome", Kind: LineNotice}))
	number := m.bufferNo["status"]
	if number == 0 {
		t.Fatal("the bare status window has no number")
	}
	if got := m.networkOf("status"); got != "" {
		t.Fatalf("setup: status already has network %q", got)
	}

	// Now a tagged line arrives and names the server.
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "welcome to libera", Kind: LineNotice}))

	key := windowKey("libera", "status")
	if _, old := m.windows["status"]; old {
		t.Fatalf("the bare key survived the migration: %v", m.order)
	}
	if got := m.bufferNo[key]; got != number {
		t.Fatalf("the number changed from %d to %d when the network was learnt", number, got)
	}
	// The earlier untagged line came with it, or the user's scrollback silently vanished at
	// the moment the client learnt where it was.
	var body strings.Builder
	for _, l := range m.windows[key] {
		body.WriteString(l.Text)
		body.WriteString("\n")
	}
	if !strings.Contains(body.String(), "an untagged welcome") {
		t.Fatalf("the pre-network scrollback was lost in the migration:\n%s", body.String())
	}
	if !strings.Contains(body.String(), "welcome to libera") {
		t.Fatalf("the tagged line did not arrive:\n%s", body.String())
	}
	// And /b still reaches it by the number the user has already learnt.
	if after := typeLine(m, "/b "+strconv.Itoa(number)); after.active != key {
		t.Fatalf("/b %d landed on %q, want %q", number, after.active, key)
	}
}

// TestTwoNetworksWithOneChannelName is the same defect one level down: two networks both hold
// "#salotto", which is the ordinary case for anyone on two servers. Their conversations must
// not interleave, and the two windows must be distinguishable in the buffer list - which says
// "status (libera)" only when it has to.
func TestTwoNetworksWithOneChannelName(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "libera", Nick: "alice", Text: "hello from libera", Kind: LineMessage}))
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "freenode", Nick: "bob", Text: "hello from freenode", Kind: LineMessage}))

	for _, c := range []struct{ key, want, unwanted string }{
		{windowKey("libera", "#salotto"), "hello from libera", "hello from freenode"},
		{windowKey("freenode", "#salotto"), "hello from freenode", "hello from libera"},
	} {
		var body strings.Builder
		for _, l := range m.windows[c.key] {
			body.WriteString(l.Text)
			body.WriteString("\n")
		}
		if !strings.Contains(body.String(), c.want) {
			t.Fatalf("%q is missing %q:\n%s", c.key, c.want, body.String())
		}
		if strings.Contains(body.String(), c.unwanted) {
			t.Fatalf("%q holds the other network's line too:\n%s", c.key, body.String())
		}
	}

	// The buffer list has to say which is which, or "/b 2" and "/b 3" both read "#salotto".
	list := m.bufferList()
	for _, want := range []string{"#salotto (libera)", "#salotto (freenode)"} {
		if !strings.Contains(list, want) {
			t.Fatalf("the buffer list does not disambiguate: %q", list)
		}
	}
	// And that printed form is a valid /go target, or the list names windows nobody can reach.
	if after := typeLine(m, "/go #salotto (freenode)"); after.active != windowKey("freenode", "#salotto") {
		t.Fatalf("/go could not follow its own buffer list: landed on %q", after.active)
	}
}

// TestAnAmbiguousNameIsNotGuessed: when two networks both hold "status", a bare "status" no
// longer names one window, and picking either is how a /go lands in the wrong server's
// window. The bare name must stay bare - it becomes its own (empty) window, which is
// visible, rather than silently opening somebody else's.
//
// The opposite would be worse than useless: a guess that is right half the time is a bug
// nobody reports, because half the time it works.
func TestAnAmbiguousNameIsNotGuessed(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "a", Kind: LineNotice}))
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "freenode", Nick: "srv", Text: "b", Kind: LineNotice}))

	if net := m.netOfName("status"); net != "" {
		t.Fatalf("a bare \"status\" with two networks resolved to %q instead of admitting it is ambiguous", net)
	}
	// And a lookup by the bare name does not silently land in one of them.
	key := m.keyFor("status")
	if key == windowKey("libera", "status") || key == windowKey("freenode", "status") {
		t.Fatalf("keyFor(\"status\") guessed %q", key)
	}
}

// TestASingleNetworkDoesNotGrowParentheses: the disambiguation is conditional. A client with
// one network - which is most of them - must not see "status (libera)" in its buffer list.
func TestASingleNetworkDoesNotGrowParentheses(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "connected", Kind: LineNotice}))
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "libera", Nick: "alice", Text: "hi", Kind: LineMessage}))

	list := m.bufferList()
	if strings.Contains(list, "(") {
		t.Fatalf("a single-network client got disambiguated names: %q", list)
	}
	if !strings.Contains(list, "status") || !strings.Contains(list, "#salotto") {
		t.Fatalf("the buffer list is missing its windows: %q", list)
	}
}
