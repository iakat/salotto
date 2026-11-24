package ui

import (
	"strings"
	"testing"
	"time"
)

// TestAnOversizedBlobIsRefusedLocally is the client half of the truncation fix.
//
// The bouncer refuses a blob it cannot read whole, so a client that sent one would earn a status
// NOTICE it could have written itself. Worse, the user's reasonable reading of that NOTICE is
// "my settings did not save" - but they did save, to the file. The honest statement is that they
// are saved here and not shared, and it has to be made by the side that knows the size.
//
// The assertion on "nothing was sent" is the one that matters: a client that reports the problem
// and sends anyway has only added a second message to the same failure.
func TestAnOversizedBlobIsRefusedLocally(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/settings.json"
	fb := newFakeBouncer(t)
	tr, err := Dial(fb.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()

	p := NewProgram("#test", tr)
	p.WithSettings(path)

	// Blow past the shared cap with a legal setting: a highlight word is settings data, so this
	// is a settings document the client has no reason to reject - only one too big to share.
	huge := strings.Repeat("w", maxPrefsToken)
	p.Key("runes", "/fset highlight "+huge)
	p.Key("enter", "")

	// Nothing may reach the bouncer.
	select {
	case line := <-fb.sent:
		if strings.HasPrefix(line, "PREF SET ") {
			t.Fatalf("the client sent a %d-byte blob the bouncer refuses: %q", len(line), trim(line, 80))
		}
		// PREF GET is fine and expected; anything else is a different bug.
		t.Fatalf("unexpected line on the wire: %q", trim(line, 80))
	case <-time.After(500 * time.Millisecond):
	}

	// And the user is told, in the status window, that the change is local only.
	view := p.View()
	if !strings.Contains(view, "not shared") {
		t.Fatalf("the client refused to share the settings silently; the view does not mention it:\n%s", trim(view, 400))
	}
}

// trim shortens a string for a failure message so a 90 KiB blob does not bury the assertion.
func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
