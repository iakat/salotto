package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// Ignore is the other half of mute, and the half that must be deliberately different. Mute
// keeps a nick's text and drops the signal; ignore keeps the line in the buffer but collapses
// its drawn form, so the conversation is still there when the rule is removed.
//
// These tests assert on the drawn frame wherever the claim is about what a user sees. A test
// that stopped at m.ignored() would pass on a renderer that never consulted it.

// ignoreFrame is a model on a visible channel, sized so the conversation is actually drawn.
func ignoreFrame(t *testing.T) Model {
	t.Helper()
	m := New("#test")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

func TestAnIgnoredNicksLinesAreNotDrawn(t *testing.T) {
	m := ignoreFrame(t)
	m.settings.Ignore = []string{"bob"}
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "bob", Text: "MUST_NOT_APPEAR", Kind: LineMessage}))
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "MUST_APPEAR", Kind: LineMessage}))

	view := plainText(m.View())
	if strings.Contains(view, "MUST_NOT_APPEAR") {
		t.Fatalf("an ignored nick's text was drawn:\n%s", view)
	}
	if !strings.Contains(view, "MUST_APPEAR") {
		t.Fatalf("the visible line beside the ignored one is missing, so the test is not seeing the conversation:\n%s", view)
	}
}

func TestHiddenLinesCollapseIntoOneRow(t *testing.T) {
	m := ignoreFrame(t)
	m.settings.Ignore = []string{"bob"}
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "before the run", Kind: LineMessage}))
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "bob", Text: "first private line", Kind: LineMessage}))
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "bob", Text: "second private line", Kind: LineMessage}))
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "after the run", Kind: LineMessage}))

	view := plainText(m.View())
	if !strings.Contains(view, "2 lines from bob hidden") {
		t.Fatalf("two hidden lines did not collapse into a counted row:\n%s", view)
	}
	if got := strings.Count(view, "hidden"); got != 1 {
		t.Fatalf("the hidden run drew %d rows, want one collapsed row:\n%s", got, view)
	}
	if strings.Contains(view, "first private line") || strings.Contains(view, "second private line") {
		t.Fatalf("a hidden line's text was drawn beside its collapsed row:\n%s", view)
	}
	if !strings.Contains(view, "before the run") || !strings.Contains(view, "after the run") {
		t.Fatalf("the rows around the hidden run are missing:\n%s", view)
	}
}

func TestAHiddenLineDoesNotRaiseActivity(t *testing.T) {
	m := ignoreFrame(t)
	m.settings.Ignore = []string{"bob"}
	m.settings.Highlights = []string{"salotto"}
	// A mention from an ignored nick is still not worth interrupting the user for, because the
	// sentence is not on screen and a badge would lead to a row they cannot read.
	m, _ = m.Step(Incoming(Line{Window: "#loud", Nick: "bob", Text: "salotto, over here", Kind: LineMessage}))
	m, _ = m.Step(Incoming(Line{Window: "#quiet", Nick: "carol", Text: "ordinary chatter", Kind: LineMessage}))

	if got := m.Unread()["#loud"]; got != 0 {
		t.Fatalf("an ignored line counted %d unread", got)
	}
	if m.Mentions()["#loud"] {
		t.Fatal("an ignored line marked its window as a mention")
	}
	if got := m.Unread()["#quiet"]; got != 1 {
		t.Fatalf("the unignored window counted %d unread, want 1", got)
	}

	m = typeLine(m, "/next")
	if got := winName(m.active); got != "#quiet" {
		t.Fatalf("/next landed on %q, so the ignored window was a jump target", got)
	}
}

func TestIgnoreMatchesAGlobMask(t *testing.T) {
	m := ignoreFrame(t)
	m.settings.Ignore = []string{"bot*", "*!*@spam.example"}
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "botulism", Text: "GLOB_HIDDEN", Kind: LineMessage}))
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "robot", Text: "NOT_AT_THE_START", Kind: LineMessage}))
	// A host rule is a mask, not a nick: the prefix keeps the user and host the window does
	// not draw, so "*!*@spam.example" has something to match against.
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "spammy", Mask: "spammy!~user@spam.example", Text: "HOST_HIDDEN", Kind: LineMessage}))

	view := plainText(m.View())
	if strings.Contains(view, "GLOB_HIDDEN") {
		t.Fatalf("bot* did not hide botulism:\n%s", view)
	}
	if strings.Contains(view, "HOST_HIDDEN") {
		t.Fatalf("*!*@spam.example did not hide a nick on that host:\n%s", view)
	}
	if !strings.Contains(view, "NOT_AT_THE_START") {
		t.Fatalf("bot* hid a nick where the pattern was not at the start:\n%s", view)
	}
}

func TestIgnoreIsCaseInsensitive(t *testing.T) {
	m := ignoreFrame(t)
	m.settings.Ignore = []string{"Bob"}
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "bob", Text: "CASE_HIDDEN", Kind: LineMessage}))

	view := plainText(m.View())
	if strings.Contains(view, "CASE_HIDDEN") {
		t.Fatalf("ignoring Bob did not hide bob:\n%s", view)
	}
}

func TestOurOwnLinesAreNeverIgnored(t *testing.T) {
	m := ignoreFrame(t)
	m.settings.Ignore = []string{"alice", "me"}
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "alice", Text: "OWN_LINE_VISIBLE", Kind: LineMessage, Own: true}))

	if view := plainText(m.View()); !strings.Contains(view, "OWN_LINE_VISIBLE") {
		t.Fatalf("an own line was hidden by the ignore list:\n%s", view)
	}

	// The optimistic echo before the bouncer confirms it is ours too: Nick is "me" and the
	// line is Pending, and hiding it would make the user's own message disappear on send.
	m, _ = m.Step(Sent{Text: "PENDING_LINE_VISIBLE"})
	if view := plainText(m.View()); !strings.Contains(view, "PENDING_LINE_VISIBLE") {
		t.Fatalf("the local echo of our own message was hidden:\n%s", view)
	}
}

func TestUnignoreWithNoArgumentClearsAndSaysHowMany(t *testing.T) {
	m := ignoreFrame(t)
	m.settings.Ignore = []string{"bob", "bot*"}
	m = typeLine(m, "/unignore")

	if len(m.settings.Ignore) != 0 {
		t.Fatalf("the bare /unignore left %v", m.settings.Ignore)
	}
	if got := systemText(m); !strings.Contains(got, "cleared 2") {
		t.Fatalf("the bare /unignore did not say how many entries it removed:\n%s", got)
	}

	// Exact removal, not a glob match: "/unignore bob*" must not take "bob" with it.
	m.settings.Ignore = []string{"bob", "bob*"}
	m = typeLine(m, "/unignore bob*")
	if len(m.settings.Ignore) != 1 || m.settings.Ignore[0] != "bob" {
		t.Fatalf("removing one mask changed the rest of the list: %v", m.settings.Ignore)
	}
}

func TestIgnorePushesThroughSaveSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	m := ignoreFrame(t).WithSettings(path)

	m, cmd := typeLineStep(m, "/ignore bot*")
	if cmd == nil {
		t.Fatal("/ignore returned no command, so the change would never reach the bouncer")
	}
	if _, ok := cmd().(PrefsPush); !ok {
		t.Fatalf("/ignore returned %T, not a PrefsPush", cmd())
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the local settings file was not written: %v", err)
	}
	if !strings.Contains(string(data), `"bot*"`) {
		t.Fatalf("the ignore mask is not in the saved document:\n%s", data)
	}

	// Listing the list is not a mutation and must not push or write.
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	m, cmd = typeLineStep(m, "/ignore")
	if cmd != nil {
		t.Fatal("a bare /ignore pushed settings, so listing the list counts as a mutation")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
		t.Fatal("a bare /ignore rewrote the settings file")
	}
}

func TestIgnoreDoesNotHideMutedText(t *testing.T) {
	m := ignoreFrame(t)
	m.settings.Mutes = []string{"bob"}
	m, _ = m.Step(Incoming(Line{Window: "#test", Nick: "bob", Text: "MUTED_BUT_DRAWN", Kind: LineMessage}))

	if view := plainText(m.View()); !strings.Contains(view, "MUTED_BUT_DRAWN") {
		t.Fatalf("a muted nick's line was hidden, so mute became ignore:\n%s", view)
	}
}

func TestMuteAndIgnoreAreSeparate(t *testing.T) {
	m := ignoreFrame(t)
	if err := m.settings.set("ignore", "bot*"); err != nil {
		t.Fatal(err)
	}
	if len(m.settings.Mutes) != 0 {
		t.Fatalf("adding an ignore also changed the mute list: %v", m.settings.Mutes)
	}
	if m.muted("botulism") {
		t.Fatal("an ignored nick is also treated as muted")
	}
	if !m.ignored("botulism") {
		t.Fatal("the ignore mask did not match")
	}

	if err := m.settings.set("mute", "loud"); err != nil {
		t.Fatal(err)
	}
	if !m.muted("loud") {
		t.Fatal("the mute did not match")
	}
	if m.ignored("loud") {
		t.Fatal("a muted nick is also treated as ignored")
	}
	if len(m.settings.Ignore) != 1 || m.settings.Ignore[0] != "bot*" {
		t.Fatalf("adding a mute changed the ignore list: %v", m.settings.Ignore)
	}
}
