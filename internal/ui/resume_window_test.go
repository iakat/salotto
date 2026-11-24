package ui

import (
	"encoding/base64"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// QUEUE item 13: "refresh after a disconnect lands on the default channel, not the one you were
// in". The settings blob is the channel the bouncer already fans out to every attached client, so
// the window a user was looking at travels in it - one string, no second protocol, and the daemon
// still parses nothing.

// chosen returns a model that has been moved to a window the way a user moves it.
func chosen(t *testing.T, window string) Model {
	t.Helper()
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = m.SetActive(window)
	return m
}

// TestTheWindowYouWereLookingAtTravelsInTheBlob.
func TestTheWindowYouWereLookingAtTravelsInTheBlob(t *testing.T) {
	m := chosen(t, "#salotto")
	if m.settings.LastWindow != m.keyFor("#salotto") {
		t.Fatalf("LastWindow = %q after switching to #salotto, want its key %q",
			m.settings.LastWindow, m.keyFor("#salotto"))
	}

	// It survives the encode/decode round trip the wire and the file both use, or it is not
	// shared at all.
	got := decodeSettings(mustBlob(t, m.settings))
	if got.LastWindow != m.settings.LastWindow {
		t.Fatalf("LastWindow = %q after a round trip, want %q", got.LastWindow, m.settings.LastWindow)
	}
}

// TestAFreshClientOpensWhereTheLastOneWas is the item itself.
func TestAFreshClientOpensWhereTheLastOneWas(t *testing.T) {
	// The first client picks a window; its blob is what the bouncer now holds.
	first := chosen(t, "#salotto")
	blob := mustBlob(t, first.settings)

	// A new client starts on its default window and adopts the blob.
	second := New("status")
	if second.active != "status" {
		t.Fatalf("the fresh model did not start on status but %q", second.active)
	}
	second, _ = second.Step(Prefs{Data: blob})
	if winName(second.active) != "#salotto" {
		t.Fatalf("the new client opened on %q, want #salotto", winName(second.active))
	}
}

// TestAClientThatHasAlreadyChosenIsNotDraggedBack.
//
// Two clients attached at once disagree about which window they are looking at by definition. A
// client that is already somewhere else must not be moved to wherever the other one was, or
// switching windows in one tab would yank the other one away from what the user is reading.
func TestAClientThatHasAlreadyChosenIsNotDraggedBack(t *testing.T) {
	first := chosen(t, "#salotto")
	blob := mustBlob(t, first.settings)

	second := chosen(t, "#other")
	second, _ = second.Step(Prefs{Data: blob})
	if winName(second.active) != "#other" {
		t.Fatalf("the second client was moved to %q; it was reading #other", winName(second.active))
	}
}

// TestARestoredWindowThatDoesNotExistYetIsStillOpened.
//
// The blob can name a window this client has never seen a line in - the bouncer's replay and the
// blob arrive in either order. Creating it is right: the lines will land in it, and opening on a
// window that is about to exist is what the user asked for. Guessing, or silently staying put,
// would put them on a window they did not choose.
func TestARestoredWindowThatDoesNotExistYetIsStillOpened(t *testing.T) {
	s := DefaultSettings()
	s.LastWindow = "libera/#nowhere"
	m := New("status")
	m, _ = m.Step(Prefs{Data: mustBlob(t, s)})
	if winName(m.active) != "#nowhere" {
		t.Fatalf("opened on %q, want #nowhere", winName(m.active))
	}
	if m.networkOf(m.active) != "libera" {
		t.Fatalf("the restored window lost its network: %q", m.networkOf(m.active))
	}
}

// TestAnEmptyLastWindowLeavesTheClientAlone: a bouncer with no opinion must not move anyone.
func TestAnEmptyLastWindowLeavesTheClientAlone(t *testing.T) {
	m := New("status")
	m, _ = m.Step(Prefs{Data: mustBlob(t, DefaultSettings())})
	if m.active != "status" {
		t.Fatalf("opened on %q with no stored window, want status", m.active)
	}
}

// TestTheOpeningWindowIsNotRecordedAsAChoice.
//
// A fresh client's first window is an artefact of startup, not a decision. Recording it would let
// a client that merely started up overwrite where the user actually was, which turns "resume
// where you left off" into "resume wherever the last thing to connect happened to default to".
func TestTheOpeningWindowIsNotRecordedAsAChoice(t *testing.T) {
	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.settings.LastWindow != "" {
		t.Fatalf("LastWindow = %q on a client that has chosen nothing", m.settings.LastWindow)
	}
	// Some traffic in another window does not count as choosing it either: the user did not
	// move, a line arrived.
	m, _ = m.Step(Incoming(Line{Window: "#salotto", Nick: "alice", Text: "hello", Kind: LineMessage}))
	if m.settings.LastWindow != "" {
		t.Fatalf("LastWindow = %q after a line arrived in another window", m.settings.LastWindow)
	}
}

// TestAStartedClientDoesNotOverwriteWhereTheUserWas.
//
// The distinction between "the window this client opened on" and "a window the user chose" is
// the whole reason windowChosen exists, and it is invisible until two clients disagree: if the
// startup window counted as a choice, a client that merely started - a second tab, a phone, a
// reconnecting process - would write its default over the window the user was actually reading,
// and the next client would resume onto the wrong one.
func TestAStartedClientDoesNotOverwriteWhereTheUserWas(t *testing.T) {
	// The user is on #salotto and that is what the bouncer holds.
	user := chosen(t, "#salotto")
	stored := mustBlob(t, user.settings)

	// A second client starts up and adopts the blob. It must move to #salotto, and having been
	// restored is not the same as having chosen: the stored value must survive untouched.
	second := New("status")
	second, _ = second.Step(Prefs{Data: stored})
	if winName(second.active) != "#salotto" {
		t.Fatalf("the second client opened on %q, want #salotto", winName(second.active))
	}
	if second.settings.LastWindow != user.settings.LastWindow {
		t.Fatalf("the stored window became %q, want it unchanged at %q",
			second.settings.LastWindow, user.settings.LastWindow)
	}

	// And a client that has chosen nothing, receiving settings that name no window, records
	// nothing of its own.
	third := New("status")
	third, _ = third.Step(Prefs{Data: mustBlob(t, DefaultSettings())})
	if third.settings.LastWindow != "" {
		t.Fatalf("a client that chose nothing recorded %q", third.settings.LastWindow)
	}
}

// mustBlob encodes settings the way the client puts them on the wire and decodes the base64
// again, so a test that adopts it goes through exactly the bytes the bouncer would carry.
func mustBlob(t *testing.T, s Settings) []byte {
	t.Helper()
	token, err := s.token()
	if err != nil {
		t.Fatal(err)
	}
	blob, err := base64.StdEncoding.DecodeString(token)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}
