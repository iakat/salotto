package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestFsetTogglesASetting is /fset, the command every weechat user reaches for first.
func TestFsetTogglesASetting(t *testing.T) {
	m := New("#test")
	if !m.settings.Timestamps {
		t.Fatal("setup: timestamps default off")
	}

	m = typeLine(m, "/fset timestamps off")
	if m.settings.Timestamps {
		t.Fatal("/fset timestamps off did not turn them off")
	}
	m = typeLine(m, "/fset timestamps on")
	if !m.settings.Timestamps {
		t.Fatal("/fset timestamps on did not turn them back on")
	}
	// A bare boolean name toggles, which is what people type.
	before := m.settings.NickColors
	m = typeLine(m, "/fset nick_colors")
	if m.settings.NickColors == before {
		t.Fatal("/fset nick_colors did not toggle")
	}
}

// TestFsetNeverReachesTheWire is the property that makes it a client setting. A bouncer has
// no "timestamps" command, so a /fset that got sent would be answered with a 421 and the
// setting would not change - a failure that looks like the feature does not exist.
func TestFsetNeverReachesTheWire(t *testing.T) {
	m := New("#test")
	if cmd := typeLineCmd(m, "/fset seconds off"); cmd != nil {
		t.Fatalf("/fset produced a command for the transport: %v", cmd())
	}
}

// TestFsetRejectsAnUnknownSetting: a typo that silently does nothing is indistinguishable
// from a setting that is not implemented.
func TestFsetRejectsAnUnknownSetting(t *testing.T) {
	m := New("#test")
	after := typeLine(m, "/fset no_such_setting on")
	lines := after.windows["#test"]
	if len(lines) == 0 || !strings.Contains(strings.ToLower(lines[len(lines)-1].Text), "no setting") {
		t.Fatalf("an unknown setting was accepted silently: %v", lines)
	}
}

// TestAliasExpandsBeforeParsing is the whole point of aliases: one implementation of /join,
// reached by two spellings.
//
// The assertion is on what leaves the model, not on the expansion function: a correct expand()
// that the Enter path never calls is an alias that does not work, and testing the function
// alone cannot tell the difference.
func TestAliasExpandsBeforeParsing(t *testing.T) {
	m := New("#test")
	m = typeLine(m, "/alias k join")
	if got := m.settings.Aliases["k"]; got != "join" {
		t.Fatalf("alias stored as %q", got)
	}

	// A typed alias must be expanded on the way out. The command the model produces is what
	// the transport would turn into a wire line.
	if cmd := typeLineCmd(m, "/k #chan"); cmd == nil {
		t.Fatal("/k produced nothing at all")
	} else if msg, ok := cmd().(Sent); !ok {
		t.Fatalf("the alias produced %T", cmd())
	} else if msg.Text != "/join #chan" {
		t.Fatalf("/k went out as %q, so the alias was not expanded", msg.Text)
	}

	// Arguments are preserved whole, including spaces: a topic or a message is free text.
	if got := m.settings.expand("/j #a b c", 0); got != "/join #a b c" {
		t.Fatalf("expansion lost the argument: %q", got)
	}
}

// TestAnAliasCycleTerminates: two aliases pointing at each other is a mistake, and the depth
// bound is what turns "the client hung" into "the client ignored it".
func TestAnAliasCycleTerminates(t *testing.T) {
	m := New("#test")
	m = typeLine(m, "/alias a b")
	m = typeLine(m, "/alias b a")
	// The only thing under test is that this returns at all.
	_ = m.settings.expand("/a hello", 0)
}

// TestAliasUnknownIsLeftAlone: an alias that is not defined must not swallow the line. A
// client that ate "/unknown" would break every command it did not know about.
func TestAliasUnknownIsLeftAlone(t *testing.T) {
	m := New("#test")
	if got := m.settings.expand("/no_such_alias x", 0); got != "/no_such_alias x" {
		t.Fatalf("an undefined alias rewrote the line to %q", got)
	}
}

// TestAltAJumpsToTheNextUnreadWindow is the hotlist.
//
// The point of a hotlist is not to show what is unread but to take you there. A jump that
// only worked for the adjacent window would leave the user hunting rows in the sidebar, which
// is the thing the key exists to avoid.
func TestAltAJumpsToTheNextUnreadWindow(t *testing.T) {
	m := windowModelFor("status", "#one", "#two", "#three")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 24})
	// Unread in the last window only: the jump has to skip the two quiet ones.
	m.unread["#three"] = 4

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	if m.active != "#three" {
		t.Fatalf("Alt-A landed on %q, want the window with unread activity", m.active)
	}
}

// TestAltAWraps is the other half: from the end of the list the next hot window is at the
// top, and a jump that stopped at the bottom would strand the user.
func TestAltAWraps(t *testing.T) {
	m := windowModelFor("status", "#one", "#two")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = m.SetActive("#two")
	m.unread["status"] = 2

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	if m.active != "status" {
		t.Fatalf("Alt-A did not wrap: landed on %q", m.active)
	}
}

// TestAltAWithNothingUnreadStaysPut: a key that jumps somewhere arbitrary when there is
// nothing to jump to is a key that loses the user's place.
func TestAltAWithNothingUnreadStaysPut(t *testing.T) {
	m := windowModelFor("status", "#one")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 24})
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	if m.active != "status" {
		t.Fatalf("Alt-A moved to %q with nothing unread", m.active)
	}
}

// TestAltAIsOffWhenTheSettingIsOff: it is a setting, so turning it off has to actually turn
// it off rather than leave a key that sometimes works.
func TestAltAIsOffWhenTheSettingIsOff(t *testing.T) {
	m := windowModelFor("status", "#one")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.settings.Hotlist = false
	m.unread["#one"] = 3
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true})
	if m.active != "status" {
		t.Fatalf("Alt-A moved to %q with the hotlist disabled", m.active)
	}
}

// TestTheSettingsScreenDrawsAndLeaves is /settings.
func TestTheSettingsScreenDrawsAndLeaves(t *testing.T) {
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 24})
	m = typeLine(m, "/settings")
	if !m.showSettings {
		t.Fatal("/settings did not open the screen")
	}
	view := plainText(m.View())
	for _, want := range []string{"timestamps", "nick colours", "scroll step", "aliases"} {
		if !strings.Contains(view, want) {
			t.Fatalf("the settings screen does not mention %q:\n%s", want, view)
		}
	}
	// The frame is still the size of the terminal: a settings screen that scrolls the
	// terminal is a screen whose bottom line the user cannot see.
	if n := len(strings.Split(m.View(), "\n")); n != 24 {
		t.Fatalf("the settings screen is %d rows in a 24-row terminal", n)
	}

	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyEsc})
	if m.showSettings {
		t.Fatal("escape did not leave the settings screen")
	}
	if m.active != "#test" {
		t.Fatalf("leaving the settings screen changed the window to %q", m.active)
	}
}

// TestTheSettingsScreenDoesNotEatTheConversation: a keystroke meant for the screen must not
// reach the input box behind it, or leaving the screen types a word nobody asked for.
func TestTheSettingsScreenDoesNotEatTheConversation(t *testing.T) {
	m := New("#test")
	m.showSettings = true
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	if string(m.input) != "" {
		t.Fatalf("a keystroke on the settings screen landed in the input: %q", string(m.input))
	}
}

// TestSpaceTogglesOnTheSettingsScreen: the screen's primary action is a toggle, and space is
// what every settings UI uses for it.
func TestSpaceTogglesOnTheSettingsScreen(t *testing.T) {
	m := New("#test")
	m.showSettings = true
	m.settingsRow = 0 // timestamps
	before := m.settings.Timestamps
	m, _ = m.Step(tea.KeyMsg{Type: tea.KeySpace})
	if m.settings.Timestamps == before {
		t.Fatal("space did not toggle the selected setting")
	}
}

// TestNickColoursAreStable is the property that makes them useful: the same person is the
// same colour on every line, in every window, across reconnects - because it is a hash of the
// name and not an allocation order.
func TestNickColoursAreStable(t *testing.T) {
	a := nickColor("alice").Render("alice")
	b := nickColor("alice").Render("alice")
	if a != b {
		t.Fatal("the same nick got two colours")
	}
	// And case-folded: on IRC "Alice" and "alice" are the same person.
	if nickColor("Alice").Render("x") != nickColor("alice").Render("x") {
		t.Fatal("nick colours are case-sensitive, so one person is two colours")
	}
	// Two different people landing on the same colour is possible - there are sixteen - but
	// the obvious neighbours should differ.
	if nickColor("alice").Render("x") == nickColor("bob").Render("x") {
		t.Log("alice and bob share a colour; the palette is small, this is not a failure")
	}
}

// TestNickColoursCanBeTurnedOff: it is a setting, and a setting that does not turn off is not
// a setting. The assertion is on the bytes the terminal is sent, because that is the only
// place the difference exists - the visible text is identical either way, which is exactly why
// a test on plainText() would pass with the setting ignored.
func TestNickColoursCanBeTurnedOff(t *testing.T) {
	render := func(colors bool) string {
		m := New("#test")
		m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 20})
		m.settings.NickColors = colors
		m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "hi", Kind: LineMessage}))
		return m.View()
	}

	want := nickColor("alice").Render("alice")
	on, off := render(true), render(false)
	if !strings.Contains(on, want) {
		t.Fatal("with nick colours on, the nick was not coloured")
	}
	if strings.Contains(off, want) {
		t.Fatal("the nick was coloured with the setting off")
	}
}

// TestSettingsSurviveARestart is what makes them settings rather than session state.
func TestSettingsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")

	m := New("#test").WithSettings(path)
	m = typeLine(m, "/fset seconds off")
	m = typeLine(m, "/alias k join")

	reloaded := New("#test").WithSettings(path)
	if reloaded.settings.Seconds {
		t.Fatal("the setting did not survive a restart")
	}
	if reloaded.settings.Aliases["k"] != "join" {
		t.Fatalf("the alias did not survive a restart: %v", reloaded.settings.Aliases)
	}
}

// TestAMissingSettingsFileIsNotAnError: the first run of a new installation has no file, and
// refusing to start over that is a strange way to greet someone.
func TestAMissingSettingsFileIsNotAnError(t *testing.T) {
	m := New("#test").WithSettings(filepath.Join(t.TempDir(), "nope", "settings.json"))
	if !m.settings.Timestamps {
		t.Fatal("a missing settings file did not fall back to the defaults")
	}
}

// TestACorruptSettingsFileFallsBack: a half-written file must not leave the client unusable.
func TestACorruptSettingsFileFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := New("#test").WithSettings(path)
	if !m.settings.Timestamps {
		t.Fatal("a corrupt settings file did not fall back to the defaults")
	}
}

// TestEverySettingIsSettable is the drift guard between the table and the setter.
//
// rows() drives the settings screen and set() drives /fset. A key present in one and not the
// other is a setting half the users cannot reach, and it looks like the screen is broken
// rather than like the command is incomplete.
func TestEverySettingIsSettable(t *testing.T) {
	s := DefaultSettings()
	for _, row := range s.rows() {
		value := "on"
		if row.kind == "int" {
			value = "4"
		}
		if err := s.set(row.key, value); err != nil {
			t.Fatalf("the screen shows %q but /fset cannot set it: %v", row.key, err)
		}
		if got := valueOf(s, row.key); got == "?" {
			t.Fatalf("%q can be set but valueOf does not know it, so the confirmation lies", row.key)
		}
	}
}

// TestTheNickColumnIsColouredToo covers the second place a nick is drawn. A member list with
// one set of colours and a conversation with another makes the two impossible to match by eye
// - which is the only thing nick colours are for.
func TestTheNickColumnIsColouredToo(t *testing.T) {
	render := func(colors bool) string {
		m := New("#test")
		m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 20})
		m.settings.NickColors = colors
		m, _ = m.Step(Nicklist{Window: "#test", Nicks: []Nick{{Name: "alice"}}})
		return m.View()
	}
	want := nickColor("alice").Render("alice")
	if on := render(true); !strings.Contains(on, want) {
		t.Fatal("the nick column is not coloured with the setting on")
	}
	if off := render(false); strings.Contains(off, want) {
		t.Fatal("the nick column is coloured with the setting off")
	}
}
