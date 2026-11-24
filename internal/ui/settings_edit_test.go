package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The settings screen used to say "edit with /fset ..." on every text row. That is honest, but
// it teaches a user that the screen is a read-only summary of something set elsewhere - and the
// screen is the discoverable half of the settings, which is the whole reason it exists.
//
// These tests are about the editor, and about the bug its absence caused: pressing space to
// "open" a rule list called set(key, "") and wiped every entry, on every client, because the
// blob is shared at the bouncer.

// editingModel opens the settings screen and selects the row with this key.
func editingModel(t *testing.T, key string) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.showSettings = true
	rows := m.settings.rows()
	for i, r := range rows {
		if r.key == key {
			m.settingsRow = i
			return m
		}
	}
	t.Fatalf("no settings row for %q", key)
	return m
}

// TestATextRowOpensAnEditorInsteadOfSayingGoElsewhere.
func TestATextRowOpensAnEditorInsteadOfSayingGoElsewhere(t *testing.T) {
	m := editingModel(t, "highlight")
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	if !m.settingsEditing {
		t.Fatalf("enter on a text row did not open the editor (hint: %q)", m.settingsHint)
	}

	// Typed runes go into the value, including spaces - which is the whole point.
	for _, r := range "big news" {
		if r == ' ' {
			m, _ = m.Step(tea.KeyMsg{Type: tea.KeySpace})
			continue
		}
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if got := string(m.settingsEdit); got != "big news" {
		t.Fatalf("typed %q into the editor, want \"big news\"", got)
	}
	// And the list is untouched while typing: no keystroke in here may destroy it.
	if len(m.settings.Highlights) != 0 {
		t.Fatalf("the highlight list changed while typing: %v", m.settings.Highlights)
	}

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	if m.settingsEditing {
		t.Fatal("enter did not close the editor")
	}
	if len(m.settings.Highlights) != 1 || m.settings.Highlights[0] != "big news" {
		t.Fatalf("highlights = %v after the edit, want [big news]", m.settings.Highlights)
	}
}

// TestEscapeAbandonsAnEdit: a half-typed rule must not be committed by leaving.
func TestEscapeAbandonsAnEdit(t *testing.T) {
	m := editingModel(t, "highlight")
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("half")})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEsc})
	if m.settingsEditing {
		t.Fatal("esc did not close the editor")
	}
	if len(m.settings.Highlights) != 0 {
		t.Fatalf("esc committed %v; an abandoned edit must change nothing", m.settings.Highlights)
	}
	if m.showSettings != true {
		t.Fatal("esc from the editor closed the whole screen; it should return to the list")
	}
}

// TestSpaceInTheEditorDoesNotClearTheList is the old bug, stated as the case that must not come
// back: the user has highlights, opens the row, presses space, and later escapes.
func TestSpaceInTheEditorDoesNotClearTheList(t *testing.T) {
	m := editingModel(t, "highlight")
	_ = m.settings.set("highlight", "alice")
	_ = m.settings.set("highlight", "bob")

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeySpace})
	if len(m.settings.Highlights) != 2 {
		t.Fatalf("a space in the editor left %v, want both words", m.settings.Highlights)
	}
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEsc})
	if len(m.settings.Highlights) != 2 {
		t.Fatalf("leaving the editor left %v, want both words", m.settings.Highlights)
	}
}

// TestABooleanStillTogglesOnSpace is the negative control for the branch above: booleans are a
// keystroke, and making every row an editor would be a worse screen than the one being fixed.
func TestABooleanStillTogglesOnSpace(t *testing.T) {
	m := editingModel(t, "timestamps")
	was := m.settings.Timestamps
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeySpace})
	if m.settingsEditing {
		t.Fatal("space on a boolean opened an editor")
	}
	if m.settings.Timestamps == was {
		t.Fatalf("space on a boolean did not toggle it (still %v)", was)
	}
}

// TestAWordCanBeRemovedWithoutClearingTheList.
//
// The screen can add a rule and, before this, had no way to take one out: the only removal was
// "" / "none" / "clear", which is the whole list. That makes the screen useless for the one
// thing a user actually does to a rule - change their mind about it.
func TestAWordCanBeRemovedWithoutClearingTheList(t *testing.T) {
	m := editingModel(t, "highlight")
	_ = m.settings.set("highlight", "alice")
	_ = m.settings.set("highlight", "bob")

	// Typed through the editor, which is the path a user takes.
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "-alice" {
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.settings.Highlights) != 1 || m.settings.Highlights[0] != "bob" {
		t.Fatalf("highlights = %v after removing alice, want [bob]", m.settings.Highlights)
	}
	if m.settingsHint != "" {
		t.Fatalf("a successful removal left the hint %q", m.settingsHint)
	}
}

// TestRemovingAWordThatIsNotThereSaysSo: silence would leave the user believing the rule is
// gone while it is still matching.
func TestRemovingAWordThatIsNotThereSaysSo(t *testing.T) {
	m := editingModel(t, "mute")
	_ = m.settings.set("mute", "carol")

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	for _, r := range "-dave" {
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})

	if len(m.settings.Mutes) != 1 || m.settings.Mutes[0] != "carol" {
		t.Fatalf("mutes = %v; a failed removal changed the list", m.settings.Mutes)
	}
	if !strings.Contains(m.settingsHint, "not in the mutes") {
		t.Fatalf("hint = %q, want it to say the word was not there", m.settingsHint)
	}
}

// TestNoneStillClearsEverything: "-word" removes one, "none" removes all. They are different
// statements and must stay different, or a user clearing the list would leave one word behind.
func TestNoneStillClearsEverything(t *testing.T) {
	m := editingModel(t, "highlight")
	_ = m.settings.set("highlight", "alice")
	_ = m.settings.set("highlight", "bob")
	if err := m.settings.set("highlight", "none"); err != nil {
		t.Fatal(err)
	}
	if len(m.settings.Highlights) != 0 {
		t.Fatalf("highlights = %v after none, want empty", m.settings.Highlights)
	}
}

// TestTheCaretIsDrawnWhereTheValueIs: the editor has to be visible on the row being edited,
// otherwise a user types into something they cannot see.
func TestTheCaretIsDrawnWhereTheValueIs(t *testing.T) {
	m := editingModel(t, "highlight")
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	view := m.settingsView()
	if !strings.Contains(view, "x█") {
		t.Fatalf("the row being edited does not show the caret: %q", view)
	}
}
