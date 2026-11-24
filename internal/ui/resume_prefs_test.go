package ui

import (
	"encoding/base64"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// QUEUE item 13, the plumbing half: the window a client was last told to show travels in the
// settings blob, and a client that adopts the blob opens there.
//
// The model tests cover the reasoning; this covers the path a real client walks - the blob arrives
// as a PREF line, the transport decodes it into a Prefs message, and the model puts the user back
// where they were instead of on whatever it happens to default to.

// prefLine builds the line a daemon sends in answer to PREF GET. The token is base64, because the
// wire is, and a test that skipped the encoding would not be testing the path.
func prefLine(nick string, blob []byte) []byte {
	return []byte(":irc.test PREF " + nick + " " + base64.StdEncoding.EncodeToString(blob))
}

// TestTheWindowIsRestoredFromTheWireBlob walks the real path from a PREF line to the model.
func TestTheWindowIsRestoredFromTheWireBlob(t *testing.T) {
	s := DefaultSettings()
	s.LastWindow = "libera/#salotto"

	tr := newTestTransport(t)
	tr.handlePrefs("nicholas " + base64.StdEncoding.EncodeToString(mustBlob(t, s)))

	var prefs Prefs
	select {
	case msg := <-tr.msgs:
		var ok bool
		prefs, ok = msg.(Prefs)
		if !ok {
			t.Fatalf("the PREF line produced %T, want Prefs", msg)
		}
	default:
		t.Fatal("the PREF line produced no message")
	}

	m := New("status")
	m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
	m, _ = m.Step(prefs)
	if m.active != "libera/#salotto" {
		t.Fatalf("the client opened on %q, want libera/#salotto", m.active)
	}
}

// TestAnEmptyDocumentLeavesTheClientWhereItIs: an empty blob is the bouncer saying it holds no
// opinion - a daemon that was just started - and must not move anyone.
func TestAnEmptyDocumentLeavesTheClientWhereItIs(t *testing.T) {
	tr := newTestTransport(t)
	tr.handlePrefs("nicholas " + base64.StdEncoding.EncodeToString([]byte("{}")))

	select {
	case msg := <-tr.msgs:
		prefs, ok := msg.(Prefs)
		if !ok {
			t.Fatalf("the empty blob produced %T, want Prefs", msg)
		}
		m := New("status")
		m, _ = m.Step(tea.WindowSizeMsg{Width: 100, Height: 30})
		m, _ = m.Step(prefs)
		if m.active != "status" {
			t.Fatalf("the client opened on %q after an empty blob, want status", m.active)
		}
	default:
		t.Fatal("an empty blob produced no message; the client would never learn it holds nothing")
	}
}

// TestABlobThatIsNotBase64IsDroppedNotDrawn.
//
// The one thing a PREF line must never become is text in a status window: a line that failed to
// decode is not settings, and showing it would put protocol noise in front of the user.
func TestABlobThatIsNotBase64IsDroppedNotDrawn(t *testing.T) {
	tr := newTestTransport(t)
	tr.handlePrefs("nicholas not-base64!!")
	select {
	case msg := <-tr.msgs:
		t.Fatalf("an undecodable PREF produced %T; it should be dropped", msg)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestTheSettingsAreAskedForOnceOnRegister.
//
// A client that never asks is a client that always starts from the defaults, which would make the
// whole blob pointless.
func TestTheSettingsAreAskedForOnceOnRegister(t *testing.T) {
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	tr.Register("nicholas", "")
	// The request goes out on our own 001, not at dial time: a bouncer that is not finished
	// registering has no stored settings to hand out yet.
	conn := fb.client(t)
	if _, err := conn.Write([]byte(":irc.test 001 nicholas :Welcome\r\n")); err != nil {
		t.Fatal(err)
	}

	// The handshake goes first; the settings request follows it, exactly once.
	deadline := time.After(2 * time.Second)
	asked := 0
	for asked == 0 {
		select {
		case line := <-fb.sent:
			if line == "PREF GET" {
				asked++
			}
		case <-deadline:
			t.Fatal("the client never asked for the stored settings")
		}
	}
	// And not twice: a second request per connection is a round trip for nothing.
	for {
		select {
		case line := <-fb.sent:
			if line == "PREF GET" {
				t.Fatal("the client asked for the settings twice on one connection")
			}
		case <-time.After(100 * time.Millisecond):
			return
		}
	}
}
