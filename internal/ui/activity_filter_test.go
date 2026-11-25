package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The buffer-list activity filter is a view over the rows sidebarRows already draws. These
// tests read the drawn frame - windowColumn through sidebarText, or View() itself - rather
// than the row structs, because a suppressed row's badge and number are invisible in a struct
// a test builds by hand and only become a claim when they are cells on screen.

// quietFilterModel is a client on its status window with one unread channel and one read
// channel. The read channel is what the filter is for: it still exists, still has its number
// and still holds its lines, and the only thing the setting changes is whether it is drawn.
func quietFilterModel(t *testing.T) Model {
	t.Helper()
	m := activityModel(t)
	m = say(m, "#quiet", "bob", "already read")
	m = say(m, "#talk", "bob", "something new")
	// Looking at #quiet is what marks it read, and going back to status leaves it that way.
	m = m.SetActive(m.keyFor("#quiet"))
	m = m.SetActive(m.keyFor("status"))
	m.settings.ActiveOnly = true
	return m
}

// TestTheFilterHidesAQuietWindow: a window with no activity is absent from the drawn list.
//
// The mutation is to ignore the setting, which puts #quiet back in the sidebar. Reading only
// m.sidebarRows would see the same rows and pass only by inspecting every row's activity, so
// the assertion is on the frame.
func TestTheFilterHidesAQuietWindow(t *testing.T) {
	m := quietFilterModel(t)
	rows := strings.Join(sidebarText(m), "\n")

	if strings.Contains(rows, "#quiet") {
		t.Fatalf("the filter drew a window with no activity:\n%s", rows)
	}
	if !strings.Contains(rows, "#talk") {
		t.Fatalf("the filter hid the window that does have activity:\n%s", rows)
	}
}

// TestTheFilterKeepsTheActiveWindow: the window the user is looking at survives even when it
// is quiet, or /close becomes a guess and the user loses sight of where they are typing.
//
// The mutation is to filter every quiet window, including the active one.
func TestTheFilterKeepsTheActiveWindow(t *testing.T) {
	m := activityModel(t)
	m = say(m, "#quiet", "bob", "already read")
	m = m.SetActive(m.keyFor("#quiet"))
	m.settings.ActiveOnly = true

	rows := strings.Join(sidebarText(m), "\n")
	if !strings.Contains(rows, "▸ 2 #quiet") {
		t.Fatalf("the filter hid the active quiet window:\n%s", rows)
	}
}

// TestTheFilterKeepsTheNetworkRowOfAShownChannel: a channel drawn under no server is a
// channel on no server, so the heading survives whenever anything under it does - even when
// the status window that forms the heading is itself quiet.
//
// The mutation is to filter every quiet status row, which leaves #talk without its "libera".
func TestTheFilterKeepsTheNetworkRowOfAShownChannel(t *testing.T) {
	m := activityModel(t)
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "freenode", Nick: "srv", Text: "connected", Kind: LineSystem}))
	m = say(m, "#talk", "bob", "something new")
	// Sit on the other network so libera's status row has no activity of its own and survives
	// only because #talk did.
	m = m.SetActive(windowKey("freenode", "status"))
	m.settings.ActiveOnly = true

	rows := strings.Join(sidebarText(m), "\n")
	if !strings.Contains(rows, "libera") {
		t.Fatalf("the filter dropped the heading of a channel it kept:\n%s", rows)
	}
	if !strings.Contains(rows, "#talk") {
		t.Fatalf("the active network's channel is missing from the frame:\n%s", rows)
	}
}

// TestTheFilterDropsANetworkRowWithNothingUnderIt: an all-quiet network's status row is
// context with nothing to give context to, so it goes with the channels it used to head.
//
// The mutation is to keep every status row unconditionally, which leaves an empty "libera"
// heading over no channels.
func TestTheFilterDropsANetworkRowWithNothingUnderIt(t *testing.T) {
	m := activityModel(t)
	m = say(m, "#read", "bob", "already read")
	m = m.SetActive(m.keyFor("#read"))
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "freenode", Nick: "srv", Text: "connected", Kind: LineSystem}))
	m, _ = m.Step(Incoming(Line{Window: "#live", Network: "freenode", Nick: "alice", Text: "new", Kind: LineMessage}))
	m = m.SetActive(windowKey("freenode", "#live"))
	m.settings.ActiveOnly = true

	rows := strings.Join(sidebarText(m), "\n")
	if strings.Contains(rows, "libera") {
		t.Fatalf("the filter kept an all-quiet network's heading:\n%s", rows)
	}
	if !strings.Contains(rows, "freenode") || !strings.Contains(rows, "#live") {
		t.Fatalf("the live network's rows did not survive the filter:\n%s", rows)
	}
}

// TestTheFilterNeverHidesTheMergeOrMonitor: both are views across buffers, not buffers, and
// activityOf has no meaning for either. Hiding one would remove the only row that reaches it.
//
// The mutation is to apply the activity test to every key without excluding the two special
// windows, which hides both the moment they are opened.
func TestTheFilterNeverHidesTheMergeOrMonitor(t *testing.T) {
	m := activityModel(t)
	m = say(m, "#a", "alice", "alpha")
	m = say(m, "#b", "bob", "bravo")
	m = typeLine(m, "/monitor")
	m = typeLine(m, "/merge #a #b")
	if !m.monitor || !m.merge {
		t.Fatalf("setup: monitor=%v merge=%v, want both open", m.monitor, m.merge)
	}
	m.settings.ActiveOnly = true

	rows := strings.Join(sidebarText(m), "\n")
	for _, name := range []string{"monitor", "merge"} {
		if !strings.Contains(rows, name) {
			t.Fatalf("the filter hid the %s view:\n%s", name, rows)
		}
	}
}

// TestTheFilterDoesNotRenumber: a hidden window keeps its number, and so do its neighbours.
// Numbers are identity, not position, so /b 3 must reach the same buffer whether or not the
// list around it was filtered.
//
// The mutation is to renumber the survivors from their drawn positions, which turns "3 #talk"
// into "2 #talk" after a quiet window is hidden.
func TestTheFilterDoesNotRenumber(t *testing.T) {
	m := quietFilterModel(t)
	rows := strings.Join(sidebarText(m), "\n")

	if strings.Contains(rows, "#quiet") {
		t.Fatalf("setup: the quiet window is still drawn:\n%s", rows)
	}
	if !strings.Contains(rows, "3 #talk") {
		t.Fatalf("the visible window lost the number it had before the filter:\n%s", rows)
	}
}

// TestTheFilterReportsWhatItHides: an empty-looking sidebar with no explanation reads as a
// client that lost its windows, so the status line says how many rows the filter suppressed.
//
// The mutation is to drop the count from the status line.
func TestTheFilterReportsWhatItHides(t *testing.T) {
	m := quietFilterModel(t)

	view := plainText(m.View())
	if !strings.Contains(view, "filtered · 1 hidden") {
		t.Fatalf("the status line does not say what the filter hid:\n%s", view)
	}
}

// TestTheFilterIsPerSettingAndPushed: the filter is presentation, so it is a setting that goes
// through saveSettings and rides the existing opaque blob to every attached client. A toggle
// that returns before saving works in this window and nowhere else.
//
// The mutation is to return before saveSettings; the file never changes and the bouncer never
// hears about it.
func TestTheFilterIsPerSettingAndPushed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	m := activityModel(t).WithSettings(path)

	m, cmd := typeLineStep(m, "/active")
	if !m.settings.ActiveOnly {
		t.Fatal("the bare /active did not turn the filter on")
	}
	if !strings.Contains(systemText(m), "active filter is on") {
		t.Fatalf("the bare /active did not report the state it chose:\n%s", systemText(m))
	}
	if cmd == nil {
		t.Fatal("/active returned no command, so the change would never reach the bouncer")
	}
	if _, ok := cmd().(PrefsPush); !ok {
		t.Fatalf("/active returned %T, not a PrefsPush", cmd())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the local settings file was not written: %v", err)
	}
	if !strings.Contains(string(data), `"active_only": true`) {
		t.Fatalf("the filter is not in the saved document:\n%s", data)
	}

	// A second toggle must save again: the setting is per command, not a first-run write.
	m, cmd = typeLineStep(m, "/active")
	if m.settings.ActiveOnly {
		t.Fatal("the second bare /active did not turn the filter off")
	}
	if cmd == nil {
		t.Fatal("turning the filter off pushed nothing")
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("the local settings file was not rewritten: %v", err)
	}
	if !strings.Contains(string(data), `"active_only": false`) {
		t.Fatalf("the second toggle did not update the saved document:\n%s", data)
	}
}

// TestTheFilterAndNextAgree is the load-bearing test: the filter and /next both ask
// activityOf, so every window /next can land on is a window the sidebar would show.
//
// It checks the frame before each jump rather than the frame after it, because the active
// window is always visible and a check made after the jump would pass even when the target
// had been hidden until the walk arrived. The mention-only window is part of the fixture so a
// second, hand-written predicate - one that looks at unread but forgets mention - turns the
// test red from either side.
func TestTheFilterAndNextAgree(t *testing.T) {
	m := activityModel(t)
	m = say(m, "#a-news", "alice", "alpha")
	m = say(m, "#c-news", "bob", "charlie")
	m = say(m, "#m-mention", "carol", "are you there")
	m = m.SetActive(m.keyFor("#m-mention"))
	m = m.SetActive(m.keyFor("status"))
	// A highlight with no unread count: activityOf still ranks it, and a filter or jump key
	// that only consults unread would treat it as quiet.
	m = m.NoteMention(m.keyFor("#m-mention"))
	m.settings.ActiveOnly = true

	visited := make(map[string]bool)
	for i := 0; i < len(m.order)+2; i++ {
		before := m.active
		rows := sidebarText(m)
		next := typeLine(m, "/next")
		if next.active == before {
			break
		}
		target := winName(next.active)
		found := false
		for _, row := range rows {
			if strings.Contains(row, target) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("/next visited %q, which the filter did not show before the jump:\n%s",
				target, strings.Join(rows, "\n"))
		}
		visited[target] = true
		m = next
	}
	for _, name := range []string{"#a-news", "#c-news", "#m-mention"} {
		if !visited[name] {
			t.Fatalf("/next never visited %q, so the two activity predicates disagree: visited %v",
				name, visited)
		}
	}
}
