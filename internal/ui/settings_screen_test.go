package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// settingsRowFor puts the cursor on a named row of the settings screen.
func settingsRowFor(t *testing.T, m Model, key string) Model {
	t.Helper()
	for i, r := range m.settings.rows() {
		if r.key == key {
			m.settingsRow = i
			return m
		}
	}
	t.Fatalf("no settings row called %q", key)
	return m
}

// TestSpaceOnATextRowDoesNotWipeIt is the bug that made the settings screen unsafe to press.
//
// Enter and Space call set(key, ""), and for a boolean "" means "toggle" while for a text row it
// means "clear the list" - which is the documented way to say "/fset highlight none". So a user
// who pressed space on the highlights row to open it lost every highlight they had. That was
// bad when settings lived in one client's file; now that the blob is shared at the bouncer it
// also wiped them on every other client the user owns, which is the kind of bug that gets blamed
// on "the sync" forever.
//
// The assertion is on both halves: the list survives, and the screen says how to edit the row
// instead of silently doing nothing.
func TestSpaceOnATextRowDoesNotWipeIt(t *testing.T) {
	for _, key := range []string{"highlight", "mute"} {
		t.Run(key, func(t *testing.T) {
			m := New("#test")
			m.settings.Highlights = []string{"salotto", "hermes"}
			m.settings.Mutes = []string{"noisy"}
			m.showSettings = true
			m = settingsRowFor(t, m, key)
			before := m.settings.Highlights
			beforeMutes := m.settings.Mutes

			for _, k := range []tea.KeyMsg{
				{Type: tea.KeySpace},
				{Type: tea.KeyEnter},
			} {
				m, _ = m.Step(k)
			}

			if len(m.settings.Highlights) != len(before) || len(m.settings.Mutes) != len(beforeMutes) {
				t.Fatalf("a keystroke on %q cleared a list: highlights %v -> %v, mutes %v -> %v",
					key, before, m.settings.Highlights, beforeMutes, m.settings.Mutes)
			}
			if !strings.Contains(m.View(), "/fset "+key) {
				t.Fatalf("the screen did not say how to edit %q; hint is %q", key, m.settingsHint)
			}
		})
	}
}

// TestABooleanRowStillToggles: the fix must not turn the primary action of the screen into a
// no-op. This is the negative control for the test above.
func TestABooleanRowStillToggles(t *testing.T) {
	m := New("#test")
	m.showSettings = true
	m = settingsRowFor(t, m, "timestamps")
	before := m.settings.Timestamps
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeySpace})
	if m.settings.Timestamps == before {
		t.Fatal("space no longer toggles a boolean row")
	}
}

// TestTheHintDoesNotOutliveTheRow: the hint describes one row, so moving the cursor must clear
// it. A stale "edit with /fset highlight" under the mutes row is a screen giving instructions
// about something else.
func TestTheHintDoesNotOutliveTheRow(t *testing.T) {
	m := New("#test")
	m.showSettings = true
	m = settingsRowFor(t, m, "highlight")
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeySpace})
	if m.settingsHint == "" {
		t.Fatal("no hint was set to begin with")
	}
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyDown})
	if m.settingsHint != "" {
		t.Fatalf("the hint survived a cursor move: %q", m.settingsHint)
	}
}

// TestTheSettingsFooterDoesNotClaimTheStateIsLocal: the footer said "client, not bouncer", which
// was true until the bouncer started holding the blob. A screen that names the wrong owner is
// how a user concludes their settings are per device and stops trusting the sync.
func TestTheSettingsFooterDoesNotClaimTheStateIsLocal(t *testing.T) {
	m := New("#test")
	m.width, m.height = 100, 30
	m.showSettings = true
	view := m.View()
	if strings.Contains(view, "client, not bouncer") {
		t.Fatal("the settings screen still says the settings are the client's alone")
	}
	if !strings.Contains(view, "bouncer") {
		t.Fatal("the settings footer does not mention where the settings are held")
	}
}
