package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The model separates two networks' windows by key, but the key is an implementation detail.
// These are the tests that keep it out of everything a user reads and off the wire, because a
// fix that leaks "libera/status" into the title bar or into a CHATHISTORY has traded one bug
// for a worse one.

// TestTheTitleShowsTheNameNotTheKey: the frame's title row is the most-read string in the
// program, and it must say "status", not the model's internal key.
func TestTheTitleShowsTheNameNotTheKey(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "connected", Kind: LineNotice}))

	if m.active != windowKey("libera", "status") {
		t.Fatalf("setup: active is %q, expected the qualified key", m.active)
	}
	view := Plain(m.View())
	first := strings.SplitN(view, "\n", 2)[0]
	if strings.Contains(first, "libera/") {
		t.Fatalf("the title row shows the internal key: %q", first)
	}
	if !strings.Contains(first, "status") {
		t.Fatalf("the title row does not name the window: %q", first)
	}
}

// TestTheSidebarShowsNamesNotKeys: two rows may both say "status" - that is what the headings
// are for - but neither may say "libera/status", which is not a window name.
func TestTheSidebarShowsNamesNotKeys(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "a", Kind: LineNotice}))
	m, _ = m.Step(Incoming(Line{Window: "status", Network: "freenode", Nick: "srv", Text: "b", Kind: LineNotice}))
	m.unread = make(map[string]int)

	joined := strings.Join(sidebarText(m), "\n")
	if strings.Contains(joined, "libera/status") || strings.Contains(joined, "freenode/status") {
		t.Fatalf("the sidebar shows an internal key:\n%s", joined)
	}
	if n := strings.Count(joined, "status"); n != 2 {
		t.Fatalf("expected one status row per network, got %d:\n%s", n, joined)
	}
}

// TestTheHistoryRequestNamesTheWindowNotTheKey: CHATHISTORY takes a window name and resolves
// the network itself. Sending "libera/status" would be a request for a window no bouncer has,
// and the failure is silence - an empty batch that reads as "there is no history", which is
// the exact lie the log-backed history work existed to stop telling.
func TestTheHistoryRequestNamesTheWindowNotTheKey(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 60, Height: 12})
	for i := 0; i < 5; i++ {
		m, _ = m.Step(Incoming(Line{Window: "status", Network: "libera", Nick: "srv", Text: "line",
			Kind: LineMessage, Time: time.Date(2026, 11, 24, 8, 0, i, 0, time.UTC)}))
	}

	var want HistoryWanted
	found := false
	for i := 0; i < 50 && !found; i++ {
		var cmd tea.Cmd
		m, cmd = m.Step(tea.KeyMsg{Type: tea.KeyPgUp})
		if cmd == nil {
			continue
		}
		if hw, ok := cmd().(HistoryWanted); ok {
			want, found = hw, true
		}
	}
	if !found {
		t.Fatal("reaching the top asked for no history")
	}
	if want.Window != "status" {
		t.Fatalf("the request names %q, want the bare window name status", want.Window)
	}
}

// TestSwitchingByBareNameReachesTheQualifiedWindow: a click, a web request or a /go names a
// window the way a user does. When only one network holds that name, that is unambiguous and
// must resolve; refusing would make every window on a tagged bouncer unreachable by name.
func TestSwitchingByBareNameReachesTheQualifiedWindow(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Network: "libera", Nick: "alice", Text: "hi", Kind: LineMessage}))
	key := windowKey("libera", "#salotto")

	if _, ok := m.windows[key]; !ok {
		t.Fatalf("setup: %q is not a window: %v", key, m.order)
	}
	if after := m.SetActive("#salotto"); after.active != key {
		t.Fatalf("SetActive(\"#salotto\") landed on %q, want %q", after.active, key)
	}
	if after := typeLine(m, "/go salotto"); after.active != key {
		t.Fatalf("/go salotto landed on %q, want %q", after.active, key)
	}
	// A name nobody holds still creates a window rather than resolving to a stranger.
	if after := m.SetActive("#nowhere"); after.active != "#nowhere" {
		t.Fatalf("SetActive of an unknown name landed on %q", after.active)
	}
}
