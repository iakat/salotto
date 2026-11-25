package ui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Mute and highlight are the last of weechat's "who is worth interrupting me for" pair.
// Both are client settings, so both are decided in the model: the transport is shared by
// every front end of one account, and a rule set in a browser must not change what a phone
// considers interesting.

func TestAMutedNickDoesNotLightTheWindowUp(t *testing.T) {
	Configure()
	m := New("#here")
	m.settings.Mutes = []string{"loud"}
	m, _ = m.Step(Incoming(Line{Window: "#elsewhere", Nick: "loud", Text: "and another thing"}))

	if got := m.Unread()["#elsewhere"]; got != 0 {
		t.Fatalf("a muted nick counted %d unread", got)
	}
}

// The line still has to be there. Mute that hides text leaves a channel looking idle while
// somebody is talking to you, and "why did nobody answer" is the worse failure.
func TestAMutedNickStillGetsDrawn(t *testing.T) {
	Configure()
	m := New("#here")
	m.settings.Mutes = []string{"loud"}
	m, _ = m.Step(Incoming(Line{Window: "#here", Nick: "loud", Text: "a visible sentence"}))
	m, _ = m.Step(Incoming(Line{Window: "#elsewhere", Nick: "loud", Text: "a suppressed sentence"}))

	if !strings.Contains(plainText(m.View()), "a visible sentence") {
		t.Fatal("the muted nick's line is not on screen, so mute hid text instead of noise")
	}
	m = m.SetActive("#elsewhere")
	if !strings.Contains(plainText(m.View()), "a suppressed sentence") {
		t.Fatal("the suppressed line is not in the window's buffer")
	}
}

// An unmuted nick in the same window still counts: a mute is one name, not a mode.
func TestMuteIsPerNick(t *testing.T) {
	Configure()
	m := New("#here")
	m.settings.Mutes = []string{"loud"}
	m, _ = m.Step(Incoming(Line{Window: "#elsewhere", Nick: "quiet", Text: "hello"}))

	if got := m.Unread()["#elsewhere"]; got != 1 {
		t.Fatalf("unread = %d, want 1 for a nick that is not muted", got)
	}
}

func TestMuteIgnoresCase(t *testing.T) {
	Configure()
	m := New("#here")
	m.settings.Mutes = []string{"Loud"}
	m, _ = m.Step(Incoming(Line{Window: "#elsewhere", Nick: "loud", Text: "hi"}))

	if got := m.Unread()["#elsewhere"]; got != 0 {
		t.Fatalf("mute is case-sensitive and counted %d", got)
	}
}

func TestAHighlightWordIsAMention(t *testing.T) {
	// In another window, because a mention in the window you are looking at is a line on
	// screen - NoteMention deliberately only marks the ones you are not looking at, and
	// a badge for something already visible is noise.
	Configure()
	m := New("#here")
	m.settings.Highlights = []string{"salotto"}
	m, _ = m.Step(Incoming(Line{Window: "#elsewhere", Nick: "bob", Text: "did anyone read the salotto spec"}))

	if !m.Mentions()["#elsewhere"] {
		t.Fatal("a highlight word did not register as a mention")
	}
}

// And in the window being looked at, a highlight still marks the line: the marking is
// what a person sees, and it must not depend on which window happens to be open. This is
// checked by comparing the frame against one built without the word, because "the frame
// has escape codes in it" is true of every frame and would pass on a broken model.
func TestAHighlightMarksTheLineInTheActiveWindow(t *testing.T) {
	Configure()
	withWord := New("#here")
	withWord.settings.Highlights = []string{"salotto"}
	withWord, _ = withWord.Step(Incoming(Line{Window: "#here", Nick: "bob", Text: "the salotto spec is up"}))
	withWord, _ = withWord.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	plain := New("#here")
	plain, _ = plain.Step(Incoming(Line{Window: "#here", Nick: "bob", Text: "the salotto spec is up"}))
	plain, _ = plain.Step(tea.WindowSizeMsg{Width: 100, Height: 30})

	if withWord.View() == plain.View() {
		t.Fatal("a highlighted line draws exactly like an ordinary one, so the highlight does nothing")
	}
}

func TestAHighlightIsAWholeWord(t *testing.T) {
	Configure()
	m := New("#here")
	m.settings.Highlights = []string{"salotto"}
	m, _ = m.Step(Incoming(Line{Window: "#elsewhere", Nick: "bob", Text: "salottozzz is a different word"}))

	if m.Mentions()["#elsewhere"] {
		t.Fatal("a highlight matched inside a longer word")
	}
}

// Our own line mentioning a highlight word must not flag itself.
func TestOurOwnLineIsNotAHighlight(t *testing.T) {
	Configure()
	m := New("#here")
	m.settings.Highlights = []string{"salotto"}
	m, _ = m.Step(Incoming(Line{Window: "#elsewhere", Nick: "me", Text: "I said salotto", Own: true}))

	if m.Mentions()["#elsewhere"] {
		t.Fatal("our own line mentioned us")
	}
}

func TestNoHighlightsMeansNoExtraMentions(t *testing.T) {
	Configure()
	m := New("#here")
	m, _ = m.Step(Incoming(Line{Window: "#elsewhere", Nick: "bob", Text: "did anyone read the salotto spec"}))

	if m.Mentions()["#elsewhere"] {
		t.Fatal("a mention was invented with no highlight words set")
	}
}

// The settings round trip: /fset is how these are reachable, and the screen is the same
// list, so a key that the screen shows and /fset rejects is a setting half-reachable.
func TestHighlightAndMuteAreSettable(t *testing.T) {
	s := DefaultSettings()
	if err := s.set("highlight", "salotto"); err != nil {
		t.Fatalf("highlight: %v", err)
	}
	if err := s.set("mute", "loud"); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if len(s.Highlights) != 1 || s.Highlights[0] != "salotto" {
		t.Fatalf("highlights = %v", s.Highlights)
	}
	if len(s.Mutes) != 1 || s.Mutes[0] != "loud" {
		t.Fatalf("mutes = %v", s.Mutes)
	}
	// Adding the same word twice must not make two rules that mean one thing.
	_ = s.set("highlight", "SALOTTO")
	if len(s.Highlights) != 1 {
		t.Fatalf("a duplicate highlight was stored: %v", s.Highlights)
	}
	// And there has to be a way to empty the list, or a mistake is permanent.
	_ = s.set("highlight", "none")
	if len(s.Highlights) != 0 {
		t.Fatalf("clearing highlights left %v", s.Highlights)
	}
	// Every row the settings screen draws must be settable by name, and vice versa.
	for _, row := range DefaultSettings().rows() {
		fresh := DefaultSettings()
		if err := fresh.set(row.key, row.value); err != nil {
			t.Errorf("the screen shows %q but /fset %s %q fails: %v", row.label, row.key, row.value, err)
		}
	}
}

// /fset answers with the new value, for every key the screen shows. A confirmation that
// says "highlight is now ?" is worse than no confirmation: it teaches the user to stop
// reading them, which is also how a real failure goes unnoticed.
func TestEverySettingReadsBack(t *testing.T) {
	for _, row := range DefaultSettings().rows() {
		s := DefaultSettings()
		if err := s.set(row.key, row.value); err != nil {
			t.Errorf("the screen shows %q as %q but /fset %s %q fails: %v",
				row.label, row.value, row.key, row.value, err)
			continue
		}
		if got := valueOf(s, row.key); got == "?" || got == "" {
			t.Errorf("%s reads back as %q, so /fset would confirm with a shrug", row.key, got)
		}
	}
}

// Every spelling an alias resolves reaches the same row's value, so "/fset dates off" is
// not a second name for a setting the screen cannot show.
func TestAliasSpellingsReadBackToo(t *testing.T) {
	for _, pair := range [][2]string{
		{"dates", "date_rule"},
		{"colours", "nick_colors"},
		{"scroll", "scroll_step"},
		{"highlights", "highlight"},
		{"mutes", "mute"},
		{"ignores", "ignore"},
	} {
		alias, row := pair[0], pair[1]
		other := DefaultSettings()
		if err := other.set(row, "on"); err != nil {
			// Not every row takes a bool; use its own displayed value instead.
			_ = other.set(row, valueOf(DefaultSettings(), row))
		}
		got := valueOf(other, alias)
		want := valueOf(other, row)
		if got != want {
			t.Errorf("%q reads back %q but the row %q reads back %q", alias, got, row, want)
		}
	}
}

// A value that is set must read back as the value that was set, not as the default.
func TestTheReadBackIsTheValueNotTheDefault(t *testing.T) {
	s := DefaultSettings()
	if err := s.set("scroll_step", "7"); err != nil {
		t.Fatal(err)
	}
	if got := valueOf(s, "scroll_step"); got != "7" {
		t.Fatalf("scroll_step reads back as %q after being set to 7", got)
	}
	if err := s.set("timestamps", "off"); err != nil {
		t.Fatal(err)
	}
	if got := valueOf(s, "timestamps"); got != "off" {
		t.Fatalf("timestamps reads back as %q after being turned off", got)
	}
}

// TestTheNickColourDoesNotEndTheLineColour pins the bug that hid every highlight.
//
// A lipgloss render ends with an ANSI reset. Coloring the nick by splicing that rendered
// string into an already-styled line therefore ends the line's own style at the nick: the
// text after it is drawn in the terminal default, so a mention highlight set on the line is
// visibly wiped by the nick that precedes the text. It was invisible in the model (the
// Mention flag was set) and invisible in a frame that only checks for escape codes, and
// obvious the moment a real terminal was asked for the cell colours.
func TestTheNickColourDoesNotEndTheLineColour(t *testing.T) {
	m := New("#salotto").WithSettings("/tmp/nowhere.json")
	m.width, m.height = 120, 30
	m.settings.NickColors = true
	m.settings.Highlights = []string{"salotto"}
	m2, _ := m.Step(Incoming{Window: "#salotto", Nick: "bob", Text: "the salotto spec", Kind: LineMessage})
	lines := m2.windows["#salotto"]
	if len(lines) != 1 || !lines[0].Mention {
		t.Fatalf("the highlight word did not mark the line: %+v", lines)
	}

	rendered := lineLinesStyled(lines[0], 60, m2.settings)
	if len(rendered) == 0 {
		t.Fatal("nothing rendered")
	}
	// The mention colour has to still be in effect when the body starts, which is the part
	// after "> ". Counting the sequence in the whole line is not enough: the old bug had
	// exactly one occurrence, at the front, and none after the nick.
	body := rendered[0]
	if i := strings.Index(body, "> "); i >= 0 {
		body = body[i:]
	}
	if !strings.Contains(body, "\x1b[1;38;5;222m") {
		t.Fatalf("the mention colour does not continue past the nick: %q", rendered[0])
	}

	// And a plain line keeps its own body colour, which the same bug had also been eating
	// for every message on every deployment with nick colours on.
	plain, _ := New("#salotto").Step(Incoming{Window: "#salotto", Nick: "bob", Text: "hello", Kind: LineMessage})
	plain.settings.NickColors = true
	out := lineLinesStyled(plain.windows["#salotto"][0], 60, plain.settings)
	if len(out) == 0 {
		t.Fatal("nothing rendered")
	}
	if b := out[0]; strings.Index(b, "> ") >= 0 && !strings.Contains(b[strings.Index(b, "> "):], "\x1b[38;5;188m") {
		t.Fatalf("the body colour stops at the nick: %q", out[0])
	}
}
