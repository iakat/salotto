package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Two defects that survived the tests written alongside the feature, both about the settings file
// rather than the settings value: a save that failed was never reported, and a window switch wrote
// the whole document to disk. Neither is visible in a test that only checks what the model holds.

// countingPath is a settings path whose directory does not exist, so every SaveSettings into it
// fails. It is how a full disk or a read-only home is simulated without touching the real one.
func unwritablePath(t *testing.T) string {
	t.Helper()
	// A path under a file, not a directory: creating it can only fail.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(f, "settings.json")
}

// TestASaveThatFailsIsReported.
//
// saveSettings used to be a value receiver that appended the failure to its own copy and returned
// nothing, so the message went into a Model that was discarded. The user's symptom was "my settings
// keep resetting", which sends them looking at the sync code while the cause is the disk.
func TestASaveThatFailsIsReported(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = m.WithSettings(unwritablePath(t))

	m = typeLine(m, "/fset timestamps off")

	body := systemText(m)
	if !strings.Contains(body, "settings not saved") {
		t.Fatalf("a failed save said nothing; the window holds:\n%s", body)
	}
	// And the change still applied in memory: reporting the failure must not undo the user's
	// intent, or a momentary disk problem would silently revert a setting too.
	if m.settings.Timestamps {
		t.Fatal("the setting did not change in memory after the save failed")
	}
}

// TestSwitchingWindowDoesNotTouchTheDisk.
//
// rememberWindow wrote the file on every window switch, so holding the down arrow in a search
// result list - which moves the view once per key - meant one full settings write per keystroke.
// The value is only read back at startup, so the write belongs with the next real change.
func TestSwitchingWindowDoesNotTouchTheDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = m.WithSettings(path)

	// A first real change writes the file, so the rest of the test measures the switch and not
	// the initial state.
	m = typeLine(m, "/fset timestamps off")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a settings change did not write the file: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// Now switch windows the way a user does, repeatedly.
	m, _ = m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "hi", Kind: LineMessage})
	for i := 0; i < 5; i++ {
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlN})
		m, _ = m.Step(tea.KeyMsg{Type: tea.KeyCtrlP})
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatalf("switching windows rewrote the settings file (before %v/%d, after %v/%d)",
			before.ModTime(), before.Size(), after.ModTime(), after.Size())
	}

	// The value is still tracked in memory, so the next real change carries it.
	if m.settings.LastWindow == "" {
		t.Fatal("the model forgot which window it is on")
	}

	// And the next real change does write it, window included.
	m = typeLine(m, "/fset seconds on")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "last_window") {
		t.Fatalf("the saved document lost the window:\n%s", data)
	}
}
