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
// The assertion is on both halves: the list survives, and the screen opens an editor on the row
// rather than doing nothing visible. (It used to point at "/fset" - the editor replaced that,
// and the pointer would now be a worse answer than the one the screen can give itself.)
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
			// An empty edit says the list is unchanged, which is the honest answer now that the
			// screen can edit in place.
			if !strings.Contains(m.settingsHint, "unchanged") {
				t.Fatalf("the screen said nothing useful after an empty edit on %q; hint is %q", key, m.settingsHint)
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

// TestTheHintDoesNotOutliveTheRow: a hint describes one row, so moving the cursor must clear it.
// A stale "nothing typed; the list is unchanged" under the mutes row is a screen giving
// instructions about something else.
//
// The hint is produced by an empty edit rather than by opening one: while the editor is open its
// hint belongs to it and must stay (see TestMovingWhileEditingDoesNotLeaveTheField), so the
// invariant being pinned here is the one for hints that live on the list.
func TestTheHintDoesNotOutliveTheRow(t *testing.T) {
	m := New("#test")
	m.showSettings = true
	m = settingsRowFor(t, m, "highlight")
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter}) // open
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter}) // commit nothing
	if m.settingsEditing {
		t.Fatal("the editor is still open, so this test is not testing a list hint")
	}
	if m.settingsHint == "" {
		t.Fatal("no hint was set to begin with")
	}
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyDown})
	if m.settingsHint != "" {
		t.Fatalf("the hint survived a cursor move: %q", m.settingsHint)
	}
}

// TestMovingWhileEditingDoesNotLeaveTheField.
//
// An arrow key while typing is either a cursor movement in the text or nothing at all - what it
// must never be is a jump to another row, which would silently redirect the rest of the typing
// into a different setting. This is the cost of making the screen editable, and it is worth
// pinning because "the arrows stopped working" is a bug report either way round.
func TestMovingWhileEditingDoesNotLeaveTheField(t *testing.T) {
	m := New("#test")
	m.showSettings = true
	m = settingsRowFor(t, m, "highlight")
	at := m.settingsRow
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyUp})
	if !m.settingsEditing {
		t.Fatal("an arrow key closed the editor")
	}
	if m.settingsRow != at {
		t.Fatalf("the selection moved to %d while typing in row %d", m.settingsRow, at)
	}
	// And the keystrokes after it still go to the same value.
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	if len(m.settings.Highlights) != 1 || m.settings.Highlights[0] != "x" {
		t.Fatalf("highlights = %v, want [x] set on the row the editor was opened on", m.settings.Highlights)
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
